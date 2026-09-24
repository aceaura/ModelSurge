package openaichat

import (
	"strings"
	"testing"
)

// R105：chat 一族的两个剩余声明字段。response_format.json_schema.description
// 与 stream_options.include_obfuscation 此前入站即蒸发，同协议回写回不来。

// json_schema.description 官方键，同协议往返不能丢。
func TestJSONSchemaDescriptionRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":{` +
		`"name":"s","description":"回执结构","schema":{"type":"object"}}}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Description != "回执结构" {
		t.Fatalf("description 没进 IR：%+v", req.ResponseFormat)
	}
	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"description":"回执结构"`) {
		t.Errorf("description 没回写：%s", out)
	}
}

// stream_options.include_obfuscation 三态贯通：显式 false 与没提不是一回事。
func TestIncludeObfuscationRoundTrip(t *testing.T) {
	req, err := New().DecodeRequest([]byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,` +
			`"stream_options":{"include_usage":true,"include_obfuscation":false}}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.IncludeObfuscation == nil || *req.IncludeObfuscation {
		t.Fatalf("显式 false 没留住：%v", req.IncludeObfuscation)
	}
	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"include_obfuscation":false`) {
		t.Errorf("include_obfuscation 没回写：%s", out)
	}
	// 没提的留 nil，出站不造键。
	req2, err := New().DecodeRequest([]byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req2.IncludeObfuscation != nil {
		t.Fatalf("没提被伪造：%v", req2.IncludeObfuscation)
	}
	out2, err := New().EncodeRequest(req2.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out2), "include_obfuscation") {
		t.Errorf("缺省字段被编造出站：%s", out2)
	}
}
