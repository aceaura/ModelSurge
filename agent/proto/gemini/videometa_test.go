package gemini

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// B7：part.videoMetadata 与 partMetadata 解码即丢的修复。
// videoMetadata 决定模型看整段还是指定片段，必须随媒体块进 IR；
// partMetadata 是客户端簿记元数据，记数供诊断报出。
func TestVideoMetadataAndPartMetadataDecode(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[
		{"fileData":{"mimeType":"video/mp4","fileUri":"https://x/v.mp4"},
		 "videoMetadata":{"startOffset":"10s","endOffset":"20s","fps":5}},
		{"inlineData":{"mimeType":"image/png","data":"AAAA"}},
		{"text":"describe","partMetadata":{"source":"cam-1"}},
		{"text":"plain"}]}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 4 {
		t.Fatalf("content = %+v", req.Messages)
	}
	v := req.Messages[0].Content[0]
	if v.Type != ir.BlockMedia || v.Media == nil {
		t.Fatalf("video part = %+v", v)
	}
	if string(v.Media.VideoMeta) != `{"startOffset":"10s","endOffset":"20s","fps":5}` {
		t.Errorf("videoMetadata = %s", v.Media.VideoMeta)
	}
	// 图片块没有 videoMetadata 槽位，不得伪造。
	img := req.Messages[0].Content[1]
	if img.Type != ir.BlockImage || img.Image == nil {
		t.Errorf("image part = %+v", img)
	}
	if req.PartMetaParts != 1 {
		t.Errorf("PartMetaParts = %d, want 1", req.PartMetaParts)
	}
}
