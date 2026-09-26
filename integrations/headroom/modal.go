package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

type modalInvocation interface {
	Get(context.Context, *modal.FunctionCallGetParams) (any, error)
	Cancel(context.Context, *modal.FunctionCallCancelParams) error
}

// A timed-out Get does not cancel remote work. Attempt cancellation with a fresh,
// bounded context; the server also rejects expired queued inputs. Cancellation
// is best-effort, and startup/idle compute can still be billed.
func awaitModal(ctx context.Context, call modalInvocation) (any, error) {
	result, err := call.Get(ctx, nil)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_ = call.Cancel(cleanup, nil)
		return nil, errors.New("private headroom unavailable or request cancelled")
	}
	return result, nil
}

func (b *bridge) callModal(ctx context.Context, path, scope string, body []byte, deadline int64) ([]byte, error) {
	if b.modalMethod == nil {
		cls, err := b.modal.Cls.FromName(ctx, b.config.ModalApp, "Headroom", &modal.ClsFromNameParams{Environment: b.config.ModalEnvironment})
		if err != nil {
			return nil, errors.New("private headroom lookup unavailable")
		}
		instance, err := cls.Instance(ctx, nil)
		if err != nil {
			return nil, errors.New("private headroom instance unavailable")
		}
		method, err := instance.Method("request")
		if err != nil {
			return nil, errors.New("private headroom method unavailable")
		}
		b.modalMethod = method
	}
	// Spawn provides a call ID for explicit cancellation. The SDK serializes these
	// primitive arguments with CBOR; no Go/Python object or credential is sent.
	call, err := b.modalMethod.Spawn(ctx, []any{path, encodeModalBody(body), scope, deadline}, nil)
	if err != nil {
		return nil, errors.New("private headroom submission unavailable")
	}
	result, err := awaitModal(ctx, call)
	if err != nil {
		return nil, err
	}
	return decodeModalReply(result, b.config.MaxBodyBytes)
}

// Lossless wire compression can keep eligible inputs below Modal Spawn's 8 KiB
// inline limit. Small inputs also negotiate packed replies: an embedding query
// is short, but its float-vector response can otherwise trigger blob storage.
// Large incompressible inputs still use the SDK's bounded blob path.
func encodeModalBody(body []byte) string {
	var packed bytes.Buffer
	writer, _ := zlib.NewWriterLevel(&packed, zlib.BestSpeed)
	_, _ = writer.Write(body)
	_ = writer.Close()
	if len(body) >= 4096 && 5+base64.StdEncoding.EncodedLen(packed.Len()) >= len(body) {
		return string(body)
	}
	return "zlib:" + base64.StdEncoding.EncodeToString(packed.Bytes())
}

func decodeModalReply(result any, limit int64) ([]byte, error) {
	raw, ok := result.(string)
	// JSON escaping can expand each UTF-8 byte to six bytes in the envelope.
	if !ok || int64(len(raw)) > 6*limit+256 {
		return nil, errors.New("invalid private headroom envelope")
	}
	if strings.HasPrefix(raw, "zlib:") {
		packed, err := base64.StdEncoding.DecodeString(raw[5:])
		if err != nil {
			return nil, errors.New("invalid private headroom encoding")
		}
		source := bytes.NewReader(packed)
		reader, err := zlib.NewReader(source)
		if err != nil {
			return nil, errors.New("invalid private headroom encoding")
		}
		decoded, err := io.ReadAll(io.LimitReader(reader, 6*limit+257))
		_ = reader.Close()
		if err != nil || int64(len(decoded)) > 6*limit+256 || source.Len() != 0 {
			return nil, errors.New("invalid private headroom encoding")
		}
		raw = string(decoded)
	}
	var reply struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if json.Unmarshal([]byte(raw), &reply) != nil {
		return nil, errors.New("invalid private headroom envelope")
	}
	if reply.Status != 200 {
		return nil, fmt.Errorf("private headroom returned status %d", reply.Status)
	}
	if int64(len(reply.Body)) > limit || !json.Valid([]byte(reply.Body)) {
		return nil, errors.New("invalid private headroom response")
	}
	return []byte(reply.Body), nil
}
