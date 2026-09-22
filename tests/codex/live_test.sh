#!/usr/bin/env bash
# Opt-in live test: consumes a small amount of the connected subscription.
set -euo pipefail
set +x
umask 077
: "${CODEX_GATEWAY_KEY:?Set the gateway virtual key, not an OpenAI token}"
: "${CODEX_ADMIN_PASSWORD:?Set the dashboard password}"
: "${HEADROOM_METRICS_TOKEN:?Set the Headroom monitoring token}"
: "${CODEX_TEST_MODEL:?Select a codex/ model from your account catalog}"
base=${CODEX_GATEWAY_URL:-http://127.0.0.1:8080}
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
started=$(date -u +%Y-%m-%dT%H:%M:%S)
jq -n --arg model "$CODEX_TEST_MODEL" '{model:$model,messages:[
  {role:"user",content:"Find the fatal transaction ID and amount. Reply only with the ID and amount."},
  {role:"assistant",tool_calls:[{id:"call_logs",type:"function",function:{name:"read_logs",arguments:"{}"}}]},
  {role:"tool",tool_call_id:"call_logs",content:(("2026-09-22 INFO request completed successfully\n" * 250)+"FATAL transaction=TX-731 amount=1949.37 failed integrity check\n")}
],tools:[{type:"function",function:{name:"read_logs",description:"Read synthetic test logs",parameters:{type:"object",properties:{},additionalProperties:false}}}]}' > "$scratch/request.json"

call() {
  curl --fail --silent --show-error --max-time 90 \
    -H "x-bf-vk: $CODEX_GATEWAY_KEY" -H 'Content-Type: application/json' \
    --data-binary @"$1" "$base/v1/${3:-chat/completions}" > "$2"
}
call "$scratch/request.json" "$scratch/unary.json"
jq -e '(.choices[0].message.content | contains("TX-731") and contains("1949.37")) and .usage.prompt_tokens > 0' "$scratch/unary.json" >/dev/null
echo 'PASS: live Chat answer retains the planted transaction and amount'
jq '{model,usage}' "$scratch/unary.json"

jq '.stream=true | .stream_options={include_usage:true}' "$scratch/request.json" > "$scratch/stream.json"
call "$scratch/stream.json" "$scratch/stream.sse"
rg '^data: ' "$scratch/stream.sse" | cut -c 7- | jq -Rsc 'split("\n") | map(select(length>0 and .!="[DONE]") | fromjson)' > "$scratch/chunks.json"
jq -e '([.[].choices[]?.delta.content // empty] | join("") | contains("TX-731") and contains("1949.37")) and any(.[]; .usage.prompt_tokens > 0)' "$scratch/chunks.json" >/dev/null
rg -q '^data: \[DONE\]' "$scratch/stream.sse"
echo 'PASS: live Chat SSE retains the answer, usage and terminal marker'

jq '{model,input:[.messages[0],{type:"function_call",call_id:"call_logs",name:"read_logs",arguments:"{}"},{type:"function_call_output",call_id:"call_logs",output:.messages[2].content}],tools:[.tools[0].function + {type:"function"}]}' "$scratch/request.json" > "$scratch/responses.json"
call "$scratch/responses.json" "$scratch/responses-result.json" responses
jq -e '([.output[]?.content[]? | select(.type=="output_text") | .text] | join("") | contains("TX-731") and contains("1949.37")) and .usage.input_tokens>0' "$scratch/responses-result.json" >/dev/null
echo 'PASS: live Responses unary retains the answer and usage'
jq '.stream=true' "$scratch/responses.json" > "$scratch/responses-stream.json"
call "$scratch/responses-stream.json" "$scratch/responses.sse" responses
rg '^data: ' "$scratch/responses.sse" | cut -c 7- | jq -Rsc 'split("\n") | map(select(length>0 and .!="[DONE]") | fromjson)' > "$scratch/response-events.json"
jq -e '([.[] | select(.type=="response.output_text.delta") | .delta] | join("") | contains("TX-731") and contains("1949.37")) and any(.[]; .type=="response.completed" and .response.usage.input_tokens>0)' "$scratch/response-events.json" >/dev/null
echo 'PASS: live Responses SSE retains the answer, usage and completion event'

curl --fail --silent --show-error --max-time 10 \
  -u "${CODEX_ADMIN_USERNAME:-codex}:$CODEX_ADMIN_PASSWORD" \
  -H "X-Headroom-Admin-Token: $HEADROOM_METRICS_TOKEN" \
  "$base/api/headroom/events" > "$scratch/events.json"
jq --arg since "$started" --arg model "${CODEX_TEST_MODEL#codex/}" '[.events[] | select(.started >= $since and .provider=="codex" and .model==$model and .status=="compressed" and .provider_failed==false and (.provider_usage.prompt_tokens // .provider_usage.input_tokens)>0 and .tool_result_estimate.before_estimated_tokens > .tool_result_estimate.after_estimated_tokens)]' "$scratch/events.json" > "$scratch/compressed.json"
jq -e 'length >= 4' "$scratch/compressed.json" >/dev/null
jq '[.[] | {status,tool_result_estimate,provider_usage,compression_ms}]' "$scratch/compressed.json"
echo 'PASS: Headroom compressed all four live requests and recorded provider usage'
echo 'Estimates are not billed savings. This fixture checks one retained fact, not general answer quality.'
