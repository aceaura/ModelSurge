package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R66：anthropic container 全链路贯通。请求侧 string 简写与 {id,skills}
// 对象两形态统一进 IR；响应侧 Message.container 与流式 message_start /
// message_delta 的容器回显（id/expires_at/skills）双向。官方 SDK 对照：
// messages.ts MessageCreateParamsContainer = ContainerParams | string；
// Container{id, expires_at, skills}；RawMessageDeltaEvent.Delta.container。

const containerReqPrefix = `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],`

func TestContainerDecodeStringForm(t *testing.T) {
	r, err := codec{}.DecodeRequest([]byte(containerReqPrefix + `"container":"ctr_abc"}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.Container == nil || r.Container.ID != "ctr_abc" {
		t.Fatalf("Container = %+v", r.Container)
	}
	if len(r.Container.Skills) != 0 || r.Container.ExpiresAt != "" {
		t.Errorf("string 形态不应带技能/过期时间：%+v", r.Container)
	}
}

func TestContainerDecodeObjectForm(t *testing.T) {
	r, err := codec{}.DecodeRequest([]byte(containerReqPrefix +
		`"container":{"id":"ctr_1","skills":[{"skill_id":"s1","type":"custom","version":"v2"},{"skill_id":"s2","type":"anthropic"}]}}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	ct := r.Container
	if ct == nil || ct.ID != "ctr_1" || len(ct.Skills) != 2 {
		t.Fatalf("Container = %+v", ct)
	}
	if ct.Skills[0] != (ir.Skill{SkillID: "s1", Type: "custom", Version: "v2"}) {
		t.Errorf("Skills[0] = %+v", ct.Skills[0])
	}
	// version 缺省（=latest）与显式值都要保真。
	if ct.Skills[1] != (ir.Skill{SkillID: "s2", Type: "anthropic"}) {
		t.Errorf("Skills[1] = %+v", ct.Skills[1])
	}
}

func TestContainerDecodeAbsentAndNull(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
		containerReqPrefix + `"container":null}`,
	} {
		r, err := codec{}.DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("DecodeRequest: %v", err)
		}
		if r.Container != nil {
			t.Errorf("缺席/null 应为 nil：%+v", r.Container)
		}
	}
}

func TestContainerDecodeMalformed(t *testing.T) {
	_, err := codec{}.DecodeRequest([]byte(containerReqPrefix + `"container":{"id":42}}`))
	if err == nil {
		t.Fatal("畸形 container 应报错")
	}
}

func TestContainerEncodeForms(t *testing.T) {
	mk := func(ct *ir.Container) *ir.Request {
		return &ir.Request{Model: "m", MaxTokens: 10, Container: ct,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	}
	// 仅 id 无技能 → string 简写形态（与对象 {id} 无 skills 语义等价，取最简）。
	out, err := codec{}.EncodeRequest(mk(&ir.Container{ID: "ctr_abc"}))
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"container":"ctr_abc"`) {
		t.Errorf("简写形态错：%s", out)
	}
	// 带技能 → 对象形态。
	out, err = codec{}.EncodeRequest(mk(&ir.Container{ID: "ctr_1",
		Skills: []ir.Skill{{SkillID: "s1", Type: "custom", Version: "v2"}}}))
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ct, ok := wire["container"].(map[string]any)
	if !ok {
		t.Fatalf("对象形态错：%s", out)
	}
	if ct["id"] != "ctr_1" {
		t.Errorf("container.id = %v", ct["id"])
	}
	skills := ct["skills"].([]any)
	s0 := skills[0].(map[string]any)
	if s0["skill_id"] != "s1" || s0["type"] != "custom" || s0["version"] != "v2" {
		t.Errorf("skills[0] = %v", s0)
	}
	// nil → 不出键。
	out, _ = codec{}.EncodeRequest(mk(nil))
	if strings.Contains(string(out), "container") {
		t.Errorf("nil 不应出键：%s", out)
	}
}

// 同族往返：对象形态 decode→encode→decode 三字段不漂移。
func TestContainerRequestRoundTrip(t *testing.T) {
	in := []byte(containerReqPrefix +
		`"container":{"id":"ctr_9","skills":[{"skill_id":"s1","type":"anthropic"}]}}`)
	r, err := codec{}.DecodeRequest(in)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := codec{}.EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	back, err := codec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	if back.Container.ID != "ctr_9" || len(back.Container.Skills) != 1 ||
		back.Container.Skills[0].SkillID != "s1" {
		t.Errorf("往返漂移：%+v", back.Container)
	}
}

// ---- 响应侧 ----

func TestContainerResponseRoundTrip(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":1,"output_tokens":1},
		"container":{"id":"ctr_7","expires_at":"2026-09-22T12:00:00Z",
		"skills":[{"skill_id":"s1","type":"custom","version":"v3"}]}}`
	resp, err := codec{}.DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	ct := resp.Container
	if ct == nil || ct.ID != "ctr_7" || ct.ExpiresAt != "2026-09-22T12:00:00Z" ||
		len(ct.Skills) != 1 || ct.Skills[0].Version != "v3" {
		t.Fatalf("Container = %+v", ct)
	}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"container":{"id":"ctr_7","expires_at":"2026-09-22T12:00:00Z","skills":[{"skill_id":"s1","type":"custom","version":"v3"}]}`) {
		t.Errorf("响应回写错：%s", out)
	}
	// 无容器 → 不出键。
	resp.Container = nil
	out, _ = codec{}.EncodeResponse(resp)
	if strings.Contains(string(out), "container") {
		t.Errorf("nil 不应出键：%s", out)
	}
	// 显式 null 解码为 nil。
	resp2, err := codec{}.DecodeResponse([]byte(
		`{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"container":null}`))
	if err != nil {
		t.Fatalf("DecodeResponse(null): %v", err)
	}
	if resp2.Container != nil {
		t.Errorf("null 应为 nil：%+v", resp2.Container)
	}
}

// 流式：message_start 与 message_delta 都可能携带容器回显。
func TestContainerStreamDecode(t *testing.T) {
	d := codec{}.NewStreamDecoder()
	evs, err := d.Feed("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":1},"container":{"id":"ctr_s","expires_at":"t1","skills":null}}}`)
	if err != nil || len(evs) != 1 {
		t.Fatalf("Feed start: %v %d", err, len(evs))
	}
	if evs[0].Container == nil || evs[0].Container.ID != "ctr_s" {
		t.Fatalf("message_start Container = %+v", evs[0].Container)
	}
	evs, err = d.Feed("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","container":{"id":"ctr_s","expires_at":"t2","skills":[{"skill_id":"s1","type":"anthropic","version":"v1"}]}},"usage":{"output_tokens":2}}`)
	if err != nil || len(evs) != 1 {
		t.Fatalf("Feed delta: %v %d", err, len(evs))
	}
	ct := evs[0].Container
	if ct == nil || ct.ExpiresAt != "t2" || len(ct.Skills) != 1 {
		t.Fatalf("message_delta Container = %+v", ct)
	}
	// 聚合：delta 晚到的后值覆盖。
	var a ir.Aggregator
	a.Feed(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Container: &ir.Container{ID: "ctr_s", ExpiresAt: "t1"}})
	a.Feed(ir.Event{Type: ir.EvMessageDelta, Container: ct})
	got, _ := a.Finish()
	if got.Container == nil || got.Container.ExpiresAt != "t2" || len(got.Container.Skills) != 1 {
		t.Errorf("聚合覆盖错：%+v", got.Container)
	}
	// 仅首帧携带（delta 不带）时聚合也必须收得到。
	var a2 ir.Aggregator
	a2.Feed(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Container: &ir.Container{ID: "ctr_only", ExpiresAt: "t0"}})
	a2.Feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	got2, _ := a2.Finish()
	if got2.Container == nil || got2.Container.ID != "ctr_only" {
		t.Errorf("首帧容器聚合丢失：%+v", got2.Container)
	}
}

func TestContainerStreamEncode(t *testing.T) {
	e := codec{}.NewStreamEncoder()
	frames, err := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m",
		Container: &ir.Container{ID: "ctr_s", ExpiresAt: "t1"}})
	if err != nil || len(frames) != 1 {
		t.Fatalf("Encode start: %v %d", err, len(frames))
	}
	if !strings.Contains(string(frames[0]), `"container":{"id":"ctr_s","expires_at":"t1","skills":null}`) {
		t.Errorf("message_start 帧：%s", frames[0])
	}
	frames, _ = e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn,
		Container: &ir.Container{ID: "ctr_s", ExpiresAt: "t1", Skills: []ir.Skill{{SkillID: "s1", Type: "custom", Version: "v1"}}}})
	if !strings.Contains(string(frames[0]), `"container":{"id":"ctr_s","expires_at":"t1","skills":[{"skill_id":"s1","type":"custom","version":"v1"}]}`) {
		t.Errorf("message_delta 帧：%s", frames[0])
	}
	// 无容器 → 帧内无 container 键。
	e2 := codec{}.NewStreamEncoder()
	frames, _ = e2.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
	if strings.Contains(string(frames[0]), "container") {
		t.Errorf("无容器不应出键：%s", frames[0])
	}
}
