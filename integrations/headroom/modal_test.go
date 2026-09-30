package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

type fakeModalCall struct {
	get    func(context.Context) (any, error)
	cancel func(context.Context) error
}

// Run this binary on the gateway host, not the orb. It measures the two remote
// stages, not the HTTP gateway, semantic-cache lookup, or upstream provider.
func TestLiveModalPipeline(t *testing.T) {
	if os.Getenv("HEADROOM_PIPELINE_LIVE") != "1" {
		t.Skip("requires explicit approval for host pipeline measurements")
	}
	samples := 10
	if value := os.Getenv("HEADROOM_PIPELINE_SAMPLES"); value != "" {
		var err error
		samples, err = strconv.Atoi(value)
		if err != nil || samples < 1 || samples > 50 {
			t.Fatal("pipeline samples must be 1..50; each sample makes two paid calls")
		}
	}
	client, err := modal.NewClient()
	if err != nil {
		t.Fatal("Modal credentials unavailable")
	}
	b := &bridge{modal: client, config: Config{ModalApp: "bifrost-headroom", ModalEnvironment: "main", MaxBodyBytes: 4 << 20, TimeoutMS: 120000, CostLedgerPath: t.TempDir() + "/cost.json"}}
	defer b.close()
	text := strings.Repeat("2026-09-22 INFO request completed successfully\n", 400) + "FATAL transaction=TX-731 amount=1949.37 failed integrity check\n"
	started := time.Now()
	baseline, tokens, err := b.compress(context.Background(), "gpt-4.1", strings.Repeat("a", 64), []string{text})
	if err != nil || len(baseline) != 1 || len(baseline[0]) >= len(text) || !strings.Contains(baseline[0], "TX-731") || !strings.Contains(baseline[0], "1949.37") {
		t.Fatal("startup compression failed reduction or protected-fact checks")
	}
	t.Logf("startup=%s tokens_before=%d tokens_after=%d compressed_sha256=%x", time.Since(started), tokens.Before, tokens.After, sha256.Sum256([]byte(baseline[0])))
	b.config.TimeoutMS = 30000
	body := []byte(`{"model":"headroom-minilm-v1","input":["Find the failed transaction and its amount in the application logs."]}`)
	var durations []time.Duration
	for sample := range samples {
		started = time.Now()
		release, err := b.admitModal(nil)
		admitted := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		plain := os.Getenv("HEADROOM_EMBEDDING_COMPARE") == "1" && sample%2 == 0
		wire := encodeModalBody(body)
		if plain {
			wire = string(body)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		deadline, _ := ctx.Deadline()
		call, err := b.modalMethod.Spawn(ctx, []any{"/v1/embeddings", wire, "", deadline.UnixMilli()}, nil)
		submitted := time.Now()
		if err != nil {
			cancel()
			release()
			t.Fatal("embedding submission failed")
		}
		result, err := awaitModal(ctx, call)
		cancel()
		release()
		embedded := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := decodeModalReply(result, b.config.MaxBodyBytes)
		if err != nil {
			t.Fatal(err)
		}
		var reply struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &reply) != nil || len(reply.Data) != 1 || len(reply.Data[0].Embedding) != 384 {
			t.Fatal("invalid embedding result")
		}
		if sample == 0 {
			t.Logf("embedding_json_bytes=%d reply_wire_bytes=%d", len(raw), len(result.(string)))
			if path := os.Getenv("HEADROOM_EMBEDDING_REFERENCE"); path != "" {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		out, _, err := b.compress(context.Background(), "gpt-4.1", strings.Repeat("a", 64), []string{text})
		finished := time.Now()
		if err != nil || len(out) != 1 || out[0] != baseline[0] {
			t.Fatal("compression failed or output changed")
		}
		durations = append(durations, finished.Sub(started))
		t.Logf("pipeline sample=%d plain=%t embedding_admission=%s spawn=%s get=%s embedding=%s compression_with_admission=%s total=%s", sample, plain, admitted.Sub(started), submitted.Sub(admitted), embedded.Sub(submitted), embedded.Sub(admitted), finished.Sub(embedded), finished.Sub(started))
	}
	slices.Sort(durations)
	median := durations[samples/2]
	if samples%2 == 0 {
		median = (durations[samples/2-1] + median) / 2
	}
	t.Logf("pipeline n=%d calls=%d min=%s median=%s max=%s; not a p99 or full gateway measurement", samples, 1+2*samples, durations[0], median, durations[samples-1])
	if path := os.Getenv("HEADROOM_QUALITY_OUTPUT"); path != "" {
		fixtures := []string{
			strings.Repeat("The report describes routine maintenance and successful background processing. ", 240) + "DO NOT delete /srv/payments.db; transaction TX-731 requires approval and amount 1949.37.",
			strings.Repeat("func process(ctx context.Context) error { return validate(ctx) }\n", 240) + "// ERROR E-731: --dry-run must remain enabled for /srv/payments.db",
			strings.Repeat("Résumé: ordinary requests completed; café status is healthy. ", 240) + "ERROR E-731: account TX-731 is NOT authorized to delete /srv/payments.db",
		}
		outputs := append([]string{}, baseline...)
		protected := [][]string{
			{"DO NOT delete", "/srv/payments.db", "TX-731", "requires approval", "1949.37"},
			{"E-731", "--dry-run", "must remain enabled", "/srv/payments.db"},
			{"E-731", "TX-731", "NOT authorized", "/srv/payments.db"},
		}
		for i, fixture := range fixtures {
			out, _, err := b.compress(context.Background(), "gpt-4.1", strings.Repeat("a", 64), []string{fixture})
			if err != nil || len(out) != 1 || len(out[0]) >= len(fixture) || !strings.Contains(out[0], "/srv/payments.db") {
				t.Fatal("quality corpus failed reduction or protected path check")
			}
			for _, fact := range protected[i] {
				if !strings.Contains(out[0], fact) {
					t.Fatalf("fixture %d lost a protected fact", i)
				}
			}
			outputs = append(outputs, out[0])
		}
		data, _ := json.Marshal(outputs)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Log("quality corpus added 3 calls; compare output file against the reference variant")
	}
}

// Ten requests total: one rejected edge-auth probe, one startup, four paired samples.
func TestLiveHTTPComparison(t *testing.T) {
	endpoint, credentials := os.Getenv("HEADROOM_HTTP_BENCH_URL"), os.Getenv("HEADROOM_HTTP_TOKEN_FILE")
	if os.Getenv("HEADROOM_MODAL_LIVE") != "1" || endpoint == "" || credentials == "" {
		t.Skip("requires explicit HTTP benchmark opt-in and temporary proxy token file")
	}
	data, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal("proxy credentials unavailable")
	}
	var token map[string]string
	if json.Unmarshal(data, &token) != nil || token["Modal-Key"] == "" || token["Modal-Secret"] == "" {
		t.Fatal("invalid proxy credential file")
	}
	httpClient := &http.Client{Timeout: 120 * time.Second}
	defer httpClient.CloseIdleConnections()
	response, err := httpClient.Get(endpoint + "/v1/compress")
	if err != nil {
		t.Fatal("edge probe failed")
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("edge auth status=%d, expected 401", response.StatusCode)
	}
	client, err := modal.NewClient()
	if err != nil {
		t.Fatal("Modal client unavailable")
	}
	b := &bridge{modal: client, config: Config{ModalApp: "bifrost-headroom", ModalEnvironment: "main", MaxBodyBytes: 4 << 20, TimeoutMS: 120000}}
	defer b.close()
	text := strings.Repeat("2026-09-22 INFO request completed successfully\n", 400) + "FATAL transaction=TX-731 amount=1949.37 failed integrity check\n"
	out, _, err := b.compress(context.Background(), "gpt-4.1", strings.Repeat("a", 64), []string{text})
	if err != nil || len(out) != 1 || !strings.Contains(out[0], "TX-731") || !strings.Contains(out[0], "1949.37") {
		t.Fatal("startup fixture failed")
	}
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-4.1", "messages": []map[string]string{{"role": "tool", "tool_call_id": "slot-0", "content": text}},
		"config":  map[string]any{"protect_recent": 0, "compress_user_messages": false},
		"gateway": map[string]any{"can_redrive": false, "can_relay_response": false, "session_affinity": false, "plugin_version": "bifrost-headroom/1"},
	})
	for sample := range 4 {
		for _, useHTTP := range []bool{true, false} {
			started := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			deadline, _ := ctx.Deadline()
			var raw []byte
			if useHTTP {
				request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/compress", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Modal-Key", token["Modal-Key"])
				request.Header.Set("Modal-Secret", token["Modal-Secret"])
				request.Header.Set("X-Headroom-Project", strings.Repeat("a", 64))
				request.Header.Set("X-Headroom-Deadline-Ms", strconv.FormatInt(deadline.UnixMilli(), 10))
				response, callErr := httpClient.Do(request)
				if callErr != nil {
					cancel()
					t.Fatal("HTTP benchmark failed")
				}
				raw, err = io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
				response.Body.Close()
				if response.StatusCode != 200 {
					cancel()
					t.Fatalf("HTTP status=%d", response.StatusCode)
				}
			} else {
				var result any
				result, err = b.modalMethod.Remote(ctx, []any{"/v1/compress", encodeModalBody(body), strings.Repeat("a", 64), deadline.UnixMilli()}, nil)
				if err == nil {
					raw, err = decodeModalReply(result, 4<<20)
				}
			}
			elapsed := time.Since(started)
			cancel()
			if err != nil {
				t.Fatal("transport benchmark failed")
			}
			var reply struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if json.Unmarshal(raw, &reply) != nil || len(reply.Messages) != 1 || reply.Messages[0].Content != out[0] {
				t.Fatal("transport changed output")
			}
			t.Logf("paired sample=%d http=%t elapsed=%s", sample, useHTTP, elapsed)
		}
	}
}

func (f fakeModalCall) Get(ctx context.Context, _ *modal.FunctionCallGetParams) (any, error) {
	return f.get(ctx)
}
func (f fakeModalCall) Cancel(ctx context.Context, _ *modal.FunctionCallCancelParams) error {
	return f.cancel(ctx)
}

func TestModalCancellationUsesLiveBoundedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := false
	_, err := awaitModal(ctx, fakeModalCall{
		get: func(ctx context.Context) (any, error) { return nil, ctx.Err() },
		cancel: func(ctx context.Context) error {
			cancelled = true
			deadline, ok := ctx.Deadline()
			if ctx.Err() != nil || !ok || time.Until(deadline) > 100*time.Millisecond {
				t.Fatal("cancellation reused expired context or lacked a bounded deadline")
			}
			return nil
		},
	})
	if err == nil || !cancelled {
		t.Fatal("timed-out call was not cancelled")
	}
	result, err := awaitModal(context.Background(), fakeModalCall{
		get:    func(context.Context) (any, error) { return "success", nil },
		cancel: func(context.Context) error { t.Fatal("cancelled successful call"); return nil },
	})
	if err != nil || result != "success" {
		t.Fatalf("unexpected result %v, %v", result, err)
	}
}

func TestModalErrorsDoNotExposeRemotePayloads(t *testing.T) {
	_, err := awaitModal(context.Background(), fakeModalCall{
		get:    func(context.Context) (any, error) { return nil, errors.New("private tool text and secret") },
		cancel: func(context.Context) error { return errors.New("another secret") },
	})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "tool text") {
		t.Fatal("remote exception leaked")
	}
}

func TestModalReplyLimitsAndStatus(t *testing.T) {
	body := `{"text":"é"}`
	encode := func(status int, body string) string {
		data, _ := json.Marshal(map[string]any{"status": status, "body": body})
		return string(data)
	}
	got, err := decodeModalReply(encode(200, body), int64(len(body)))
	if err != nil || string(got) != body {
		t.Fatalf("valid boundary response rejected: %v", err)
	}
	for _, result := range []any{
		encode(200, body+" "), encode(408, body), encode(500, "sensitive exception"),
		encode(200, "not json"), 123, `{"body":"{}"}`, "{", strings.Repeat("x", 4096),
	} {
		if _, err := decodeModalReply(result, int64(len(body))); err == nil {
			t.Fatalf("accepted malformed, failed, or oversized reply: %v", result)
		}
	}
}

func TestModalWireCompression(t *testing.T) {
	noise := make([]byte, 20000)
	_, _ = rand.New(rand.NewSource(731)).Read(noise)
	for _, body := range [][]byte{noise} {
		if got := encodeModalBody(body); got != string(body) {
			t.Fatal("large incompressible body expanded")
		}
	}
	body := []byte(strings.Repeat("INFO completed é\n", 1000) + "FATAL TX-731 amount=1949.37")
	encoded := encodeModalBody(body)
	if !strings.HasPrefix(encoded, "zlib:") || len(encoded) > 7000 {
		t.Fatal("compressible body did not leave room for inline RPC envelope")
	}
	packed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(encoded, "zlib:"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := zlib.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	decoded, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(decoded, body) {
		t.Fatal("wire compression changed input")
	}
}

func TestModalSmallRequestsNegotiatePackedReplies(t *testing.T) {
	noise := make([]byte, 4095)
	_, _ = rand.New(rand.NewSource(932)).Read(noise)
	for _, body := range [][]byte{[]byte("{}"), []byte(`{"model":"headroom-minilm-v1","input":"hello"}`), noise} {
		wire := encodeModalBody(body)
		if !strings.HasPrefix(wire, "zlib:") || len(wire) > 7000 {
			t.Fatal("small request failed to opt into packed embedding replies below inline limit")
		}
		packed, err := base64.StdEncoding.DecodeString(wire[5:])
		if err != nil {
			t.Fatal(err)
		}
		reader, err := zlib.NewReader(bytes.NewReader(packed))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || !bytes.Equal(decoded, body) {
			t.Fatal("small request changed")
		}
	}
}

func TestModalPackedReplyBounds(t *testing.T) {
	body := `{"text":"` + strings.Repeat("é", 3000) + `"}`
	envelope, _ := json.Marshal(map[string]any{"status": 200, "body": body})
	wire := encodeModalBody(envelope)
	got, err := decodeModalReply(wire, int64(len(body)))
	if err != nil || string(got) != body {
		t.Fatal("packed reply changed output", err)
	}
	for _, input := range []string{wire[:len(wire)-4], "zlib:!", encodeModalBody(bytes.Repeat([]byte("x"), 20000))} {
		if _, err := decodeModalReply(input, 1000); err == nil {
			t.Fatal("accepted invalid or oversized packed reply")
		}
	}
	if _, err := decodeModalReply(wire, int64(len(body)-1)); err == nil {
		t.Fatal("decoded body limit ignored")
	}
}

// Two paid, synthetic GPU calls. Explicit opt-in; never part of normal tests.
func TestLivePrivateModal(t *testing.T) {
	if os.Getenv("HEADROOM_MODAL_LIVE") != "1" {
		t.Skip("requires explicit approval for private GPU calls")
	}
	warmSamples := 0
	if value := os.Getenv("HEADROOM_MODAL_SAMPLES"); value != "" {
		var err error
		warmSamples, err = strconv.Atoi(value)
		if err != nil || warmSamples < 1 || warmSamples > 18 {
			t.Fatal("warm samples must be 1..18 (20 total including startup)")
		}
	}
	planned := 2 + warmSamples
	if os.Getenv("HEADROOM_MODAL_PROFILE") == "1" {
		planned += 4
		if os.Getenv("HEADROOM_MODAL_REMOTE_PROFILE") == "1" {
			planned += 4
		}
	}
	if planned > 20 {
		t.Fatal("benchmark exceeds 20 total requests; reduce sample count or disable profile modes")
	}
	client, err := modal.NewClient()
	if err != nil {
		t.Fatal("Modal credentials unavailable")
	}
	// Allow scheduling plus model initialization in this deployment probe only.
	// Production compression retains its short fail-open request deadline.
	b := &bridge{modal: client, config: Config{ModalApp: "bifrost-headroom", ModalEnvironment: "main", MaxBodyBytes: 4 << 20, TimeoutMS: 120000}}
	defer b.close()
	text := strings.Repeat("2026-09-22 INFO request completed successfully\n", 400) + "FATAL transaction=TX-731 amount=1949.37 failed integrity check\n"
	var first string
	for sample := range 2 {
		started := time.Now()
		out, _, err := b.compress(context.Background(), "gpt-4.1", strings.Repeat("a", 64), []string{text})
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || len(out[0]) >= len(text) || !strings.Contains(out[0], "TX-731") || !strings.Contains(out[0], "1949.37") {
			t.Fatal("private compression failed reduction or sentinel checks")
		}
		if sample == 0 {
			first = out[0]
		} else if out[0] != first {
			t.Fatal("warm output differs from first invocation")
		}
		t.Logf("private GPU sample=%d elapsed=%s input_bytes=%d output_bytes=%d; sample 1 reuses client and method", sample, time.Since(started), len(text), len(out[0]))
	}
	if n := warmSamples; n > 0 {
		b.config.TimeoutMS = 30000
		durations := make([]time.Duration, 0, n)
		for sample := range n {
			started := time.Now()
			out, _, err := b.compress(context.Background(), "gpt-4.1", strings.Repeat("a", 64), []string{text})
			elapsed := time.Since(started)
			if err != nil || len(out) != 1 || out[0] != first {
				t.Fatalf("warm sample %d failed: %v", sample, err)
			}
			durations = append(durations, elapsed)
			t.Logf("warm sample=%d elapsed=%s", sample, elapsed)
		}
		slices.Sort(durations)
		t.Logf("warm summary n=%d min=%s median=%s max=%s; insufficient for p99", n, durations[0], durations[(n-1)/2], durations[n-1])
	}
	if os.Getenv("HEADROOM_MODAL_PROFILE") != "1" {
		return
	}
	// Four additional paid calls: interleave raw/packed on the same warm worker.
	body, err := json.Marshal(map[string]any{
		"model": "gpt-4.1", "messages": []map[string]string{{"role": "tool", "tool_call_id": "slot-0", "content": text}},
		"config":  map[string]any{"protect_recent": 0, "compress_user_messages": false},
		"gateway": map[string]any{"can_redrive": false, "can_relay_response": false, "session_affinity": false, "plugin_version": "bifrost-headroom/1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for sample := range 4 {
		started := time.Now()
		wire := string(body)
		if sample%2 == 1 {
			wire = encodeModalBody(body)
		}
		encoded := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		deadline, _ := ctx.Deadline()
		call, err := b.modalMethod.Spawn(ctx, []any{"/v1/compress", wire, strings.Repeat("a", 64), deadline.UnixMilli()}, nil)
		submitted := time.Now()
		if err != nil {
			cancel()
			t.Fatal("profile submission failed")
		}
		result, err := awaitModal(ctx, call)
		finished := time.Now()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := decodeModalReply(result, b.config.MaxBodyBytes)
		if err != nil {
			t.Fatal(err)
		}
		var reply struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(raw, &reply) != nil || len(reply.Messages) != 1 || reply.Messages[0].Content != first {
			t.Fatal("profile changed compression output")
		}
		t.Logf("profile sample=%d packed=%t raw_bytes=%d wire_bytes=%d encode=%s spawn=%s get=%s total=%s", sample, sample%2 == 1, len(body), len(wire), encoded.Sub(started), submitted.Sub(encoded), finished.Sub(submitted), finished.Sub(started))
	}
	if os.Getenv("HEADROOM_MODAL_REMOTE_PROFILE") != "1" {
		return
	}
	// Benchmark only: Remote lacks the durable cancellation handle used in production.
	for _, packed := range []bool{false, true} {
		durations := make([]time.Duration, 0, 2)
		for sample := range 2 {
			started := time.Now()
			wire := string(body)
			if packed {
				wire = encodeModalBody(body)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			deadline, _ := ctx.Deadline()
			result, err := b.modalMethod.Remote(ctx, []any{"/v1/compress", wire, strings.Repeat("a", 64), deadline.UnixMilli()}, nil)
			elapsed := time.Since(started)
			cancel()
			if err != nil {
				t.Fatal("remote benchmark failed")
			}
			raw, err := decodeModalReply(result, b.config.MaxBodyBytes)
			if err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if json.Unmarshal(raw, &reply) != nil || len(reply.Messages) != 1 || reply.Messages[0].Content != first {
				t.Fatal("remote output changed")
			}
			durations = append(durations, elapsed)
			t.Logf("remote packed=%t sample=%d elapsed=%s", packed, sample, elapsed)
		}
		slices.Sort(durations)
		t.Logf("remote summary packed=%t n=2 min=%s max=%s; insufficient for p99", packed, durations[0], durations[1])
	}
}
