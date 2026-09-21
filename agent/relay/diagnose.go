package relay

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// writeLossyNotes 把有损诊断落到日志与响应头。须在 WriteHeader 之前调用。
func writeLossyNotes(w http.ResponseWriter, target string, notes []string) {
	if len(notes) == 0 {
		return
	}
	joined := strings.Join(notes, "; ")
	log.Printf("relay: upstream %s lossy conversion: %s", target, joined)
	w.Header().Set("X-ModelSurge-Notes", joined)
}

// countMedia 按大类累计附件。nil 也计入 MediaOther：块类型已经是 media，
// 载荷却没有内容，这本身就是该丢的东西，静默跳过会漏报。
func countMedia(m *ir.Media, into map[ir.MediaKind]int) {
	if m == nil {
		into[ir.MediaOther]++
		return
	}
	into[m.Kind]++
}

// mediaNotes 逐大类比对上游能力。按大类分别报而不是合成一条，是因为读者的
// 下一步动作不同：音频装不下要先转文字，PDF 装不下要先抽文本，认不出大类的
// 附件则要先确认 MIME。降级成占位文本块的事实一并说明，否则客户端会以为
// 附件原样送达了。
func mediaNotes(counts map[ir.MediaKind]int, caps proto.Capabilities) []string {
	var notes []string
	for _, c := range []struct {
		kind ir.MediaKind
		ok   bool
		what string
	}{
		{ir.MediaDocument, caps.Documents, "document"},
		{ir.MediaAudio, caps.Audio, "audio attachment"},
		{ir.MediaVideo, caps.Video, "video attachment"},
		// other 大类没有对应能力位：MIME 认不出来就无法判断上游能否承载，
		// 一律按文档能力放行（文档槽位是各协议里最宽松的不透明容器）。
		{ir.MediaOther, caps.Documents, "attachment of unrecognized type"},
	} {
		if n := counts[c.kind]; n > 0 && !c.ok {
			notes = append(notes, fmt.Sprintf(
				"replaced %d %s(s) with a placeholder text block: upstream protocol has no slot for it", n, c.what))
		}
	}
	return notes
}

// Diagnose 对比请求特征与上游协议能力，返回本次转换必然发生的有损点描述。
// protoName 为上游协议名（codec.Name），用于判断 thinking 签名能否在该上游回放。
// 目的是让有损转换可观测（日志 + X-ModelSurge-Notes），而不是静默丢信息
// （参考 new-api RequestResult.Diagnostics 的思想）。
func Diagnose(req *ir.Request, protoName string, caps proto.Capabilities) []string {
	var notes []string

	sigs, foreign, images, urlImages, errResults := 0, 0, 0, 0, 0
	media := map[ir.MediaKind]int{}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockThinking:
				if b.Thinking != nil && b.Thinking.Signature != "" {
					sigs++
					// 签名的协议形态与上游不一致（含来源不明）：
					// codec 会保守降级，否则上游签名校验会 400
					if b.Thinking.SignatureFrom != protoName {
						foreign++
					}
				}
			case ir.BlockImage:
				images++
				if b.Image != nil && b.Image.Data == "" && b.Image.URL != "" {
					urlImages++
				}
			case ir.BlockMedia:
				countMedia(b.Media, media)
			case ir.BlockToolResult:
				if b.ToolResult == nil {
					continue
				}
				if b.ToolResult.IsError {
					errResults++
				}
				// tool 结果内嵌的附件与顶层同样会被降级，漏掉这层会让
				// 「工具返回了 PDF」这类丢失完全不可见。
				for _, c := range b.ToolResult.Content {
					if c.Type == ir.BlockMedia {
						countMedia(c.Media, media)
					}
				}
			}
		}
	}
	if sigs > 0 && !caps.ThinkingSignature {
		notes = append(notes, fmt.Sprintf("dropped %d thinking signature(s): upstream protocol cannot replay them", sigs))
	} else if foreign > 0 {
		notes = append(notes, fmt.Sprintf("dropped %d thinking signature(s): not issued by a %s-protocol upstream; replaying them cross-protocol would be rejected", foreign, protoName))
	}
	if images > 0 && !caps.Images {
		notes = append(notes, fmt.Sprintf("dropped %d image(s): upstream protocol has no image input", images))
	} else if urlImages > 0 && !caps.ImageURLs {
		// 只有形态装不下：上游收 base64 不收远程 URL（kiro）。与整协议无图片能力
		// 分开报，是因为读者的下一步动作不同——这里换成 base64 内联即可。
		notes = append(notes, fmt.Sprintf("dropped %d image(s): upstream accepts inline base64 only, not remote URLs", urlImages))
	}
	notes = append(notes, mediaNotes(media, caps)...)
	if errResults > 0 && !caps.ToolResultError {
		// 失败的工具结果在上游看来与成功结果同形，模型会把报错文本当成
		// 正常返回值继续推理。读者能做的是把失败信息写进结果文本本身。
		notes = append(notes, fmt.Sprintf("dropped error flag on %d tool result(s): upstream protocol cannot mark a tool call as failed", errResults))
	}
	if !caps.Sampling {
		var params []string
		if req.Temperature != nil {
			params = append(params, "temperature")
		}
		if req.TopP != nil {
			params = append(params, "top_p")
		}
		if len(req.StopSequences) > 0 {
			params = append(params, "stop_sequences")
		}
		if req.MaxTokens > 0 {
			params = append(params, "max_tokens")
		}
		if len(params) > 0 {
			notes = append(notes, "dropped sampling parameter(s) "+strings.Join(params, ",")+": upstream payload has no such fields")
		}
	}
	if req.TopK != nil && !caps.TopK {
		notes = append(notes, "dropped top_k: upstream protocol has no equivalent field")
	}
	if req.ResponseFormat != nil && !caps.StructuredOutput {
		// 这一条比别的更要紧：客户端会直接 JSON.parse 响应，拿到自由文本就是
		// 硬失败而非降级。读者能做的是把 schema 写进 system 提示自行约束。
		what := "JSON output mode"
		if req.ResponseFormat.IsSchema() {
			what = "JSON schema constraint"
		}
		notes = append(notes, "dropped "+what+": upstream payload has no structured output field, the response will be free-form text")
	}
	if req.ToolChoice != nil && req.ToolChoice.DisableParallel && !caps.ParallelToolCalls {
		notes = append(notes, "dropped parallel tool call restriction: upstream protocol cannot express it")
	}
	if req.Thinking != nil && req.Thinking.Enabled && req.ToolChoice != nil &&
		(req.ToolChoice.Mode == ir.ChoiceAny || req.ToolChoice.Mode == ir.ChoiceTool) &&
		!caps.ThinkingForcedToolChoice {
		notes = append(notes, "downgraded tool_choice to auto: upstream rejects forced tool choice while thinking is enabled")
	}

	var dropped, unmapped []string
	for _, t := range req.Tools {
		if t.Hosted == "" {
			continue
		}
		switch {
		case !caps.HostedTools:
			dropped = append(dropped, t.Hosted)
		case t.Hosted != ir.HostedWebSearch && t.Hosted != ir.HostedCodeExecution:
			unmapped = append(unmapped, t.Hosted) // 无跨协议映射的种类，即使上游支持托管工具也只能透传同族
		}
	}
	if len(dropped) > 0 {
		notes = append(notes, "dropped hosted tool(s) "+strings.Join(dropped, ",")+": upstream protocol cannot execute them")
	}
	if len(unmapped) > 0 {
		notes = append(notes, "no cross-protocol mapping for hosted tool(s) "+strings.Join(unmapped, ","))
	}
	return notes
}
