// Package dynar provides an in-process, SQLite-backed DynamoDB for local
// development and tests. It implements the DynamoDB HTTP/JSON protocol
// inside an aws.HTTPClient, so the ordinary AWS SDK for Go v2 client
// works against it unchanged.
package dynar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// HTTPClient returns an aws.HTTPClient that dispatches DynamoDB requests
// to this database entirely in-process. Pass it to the SDK:
//
//	client := dynamodb.New(dynamodb.Options{
//	    Region:      "ap-northeast-1",
//	    Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
//	    HTTPClient:  db.HTTPClient(),
//	})
//
// The returned client never touches the network, does not modify
// http.DefaultClient or http.DefaultTransport, and stops working when
// the DB is closed.
func (db *DB) HTTPClient() aws.HTTPClient {
	return &httpClient{db: db}
}

type httpClient struct {
	db     *DB
	nextID atomic.Uint64
}

func (c *httpClient) Do(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: err}
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: err}
	}
	req.Body.Close()

	respond := func(status int, payload any) *http.Response {
		return c.respond(req, status, payload)
	}

	if c.db.isClosed() {
		return respond(400, errorPayload("dynar#DatabaseClosedError",
			"dynar: the database is closed")), nil
	}

	target := req.Header.Get("X-Amz-Target")
	if target == "" {
		return respond(400, errorPayload("ValidationException",
			"dynar: missing X-Amz-Target header")), nil
	}
	// "DynamoDB_20120810.PutItem" -> "PutItem"
	op := target
	if i := lastIndexByte(target, '.'); i >= 0 {
		op = target[i+1:]
	}

	var input map[string]any
	if len(body) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&input); err != nil {
			return respond(400, errorPayload("ValidationException",
				fmt.Sprintf("dynar: malformed request body: %v", err))), nil
		}
	} else {
		input = map[string]any{}
	}

	ctx := req.Context()
	out, aerr := c.dispatch(ctx, op, input)
	if aerr != nil {
		return respond(400, errorPayloadFrom(aerr)), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: err}
	}
	return respond(200, out), nil
}

func (c *httpClient) respond(req *http.Request, status int, payload any) *http.Response {
	var buf bytes.Buffer
	if payload != nil {
		json.NewEncoder(&buf).Encode(payload)
	}
	id := c.nextID.Add(1)
	h := http.Header{}
	h.Set("Content-Type", "application/x-amz-json-1.0")
	h.Set("x-amzn-RequestId", fmt.Sprintf("dynar-%012d", id))
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(&buf),
		ContentLength: int64(buf.Len()),
		Request:       req,
	}
}

// errorPayload builds the DynamoDB error body. The full namespace prefix
// mirrors what real DynamoDB returns; the SDK strips it when decoding.
func errorPayload(typ, msg string) map[string]any {
	full := typ
	if lastIndexByte(typ, '#') < 0 && lastIndexByte(typ, ':') < 0 {
		full = "com.amazonaws.dynamodb.v20120810#" + typ
	}
	return map[string]any{"__type": full, "message": msg}
}

func errorPayloadFrom(e *apiError) map[string]any {
	p := errorPayload(e.typ, e.msg)
	if e.item != nil {
		p["Item"] = e.item
	}
	return p
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// dispatch routes one operation. Unknown operations fail explicitly with
// a non-retryable 400 rather than falling back anywhere.
func (c *httpClient) dispatch(ctx context.Context, op string, in map[string]any) (any, *apiError) {
	switch op {
	case "CreateTable":
		return c.db.opCreateTable(ctx, in)
	case "DescribeTable":
		return c.db.opDescribeTable(ctx, in)
	case "ListTables":
		return c.db.opListTables(ctx, in)
	case "DeleteTable":
		return c.db.opDeleteTable(ctx, in)
	case "PutItem":
		return c.db.opPutItem(ctx, in)
	case "GetItem":
		return c.db.opGetItem(ctx, in)
	case "UpdateItem":
		return c.db.opUpdateItem(ctx, in)
	case "DeleteItem":
		return c.db.opDeleteItem(ctx, in)
	case "Query":
		return c.db.opQuery(ctx, in)
	case "Scan":
		return c.db.opScan(ctx, in)
	case "DescribeEndpoints":
		return map[string]any{"Endpoints": []any{
			map[string]any{"Address": "localhost", "CachePeriodInMinutes": 1440},
		}}, nil
	case "ListTagsOfResource":
		return c.db.opListTagsOfResource(ctx, in)
	case "TagResource":
		return c.db.opTagResource(ctx, in)
	case "UntagResource":
		return c.db.opUntagResource(ctx, in)
	default:
		return nil, errValidation("dynar: operation %s is not supported", op)
	}
}
