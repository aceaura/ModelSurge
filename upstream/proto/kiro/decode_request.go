// decode_request.go Kiro generateAssistantResponse 载荷 -> IR 请求。
// 仅管理面与测试用（主路径是 EncodeRequest 方向）。
// 有损说明：BuildPayload 会把系统提示并入历史首条 user 文本，
// DecodeRequest 无法还原独立 System 字段；合成占位文本也不剥离。
package kiro

import (
	"encoding/json"
	"fmt"

	"github.com/aceaura/ModelSurge/upstream/ir"
)

// kiroImageJSON Kiro 图片条目外形。
type kiroImageJSON struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

// kiroToolResultJSON Kiro toolResults 条目外形。
type kiroToolResultJSON struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Status    string `json:"status"`
	ToolUseID string `json:"toolUseId"`
}

// kiroToolUseJSON Kiro toolUses 条目外形。
type kiroToolUseJSON struct {
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"toolUseId"`
}

// kiroReqPayload DecodeRequest 用的请求外形（只列关心的字段）。
type kiroReqPayload struct {
	ProfileArn string `json:"profileArn"`
	ConvState  struct {
		ConversationID string `json:"conversationId"`
		CurrentMessage struct {
			UserInputMessage struct {
				Content string          `json:"content"`
				ModelID string          `json:"modelId"`
				Images  []kiroImageJSON `json:"images"`
				Context struct {
					Tools []struct {
						ToolSpecification struct {
							Name        string `json:"name"`
							Description string `json:"description"`
							InputSchema struct {
								JSON json.RawMessage `json:"json"`
							} `json:"inputSchema"`
						} `json:"toolSpecification"`
					} `json:"tools"`
					ToolResults []kiroToolResultJSON `json:"toolResults"`
				} `json:"userInputMessageContext"`
			} `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []json.RawMessage `json:"history"`
	} `json:"conversationState"`
	AdditionalFields map[string]struct {
		Effort string `json:"effort"`
	} `json:"additionalModelRequestFields"`
}

// DecodeRequest Kiro 载荷 -> IR。
func (Codec) DecodeRequest(body []byte) (*ir.Request, error) {
	var p kiroReqPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("kiro decode request: %w", err)
	}
	cur := p.ConvState.CurrentMessage.UserInputMessage
	req := &ir.Request{Model: cur.ModelID}
	if p.ProfileArn != "" {
		req.Metadata = map[string]string{"kiro_profile_arn": p.ProfileArn}
	}
	if effort := effortFromAdditionalFields(p.AdditionalFields); effort != "" {
		req.Thinking = &ir.ThinkingConfig{Enabled: true, Effort: effort}
	}

	for _, raw := range p.ConvState.History {
		m, err := decodeHistoryEntry(raw)
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, *m)
	}
	req.Messages = append(req.Messages, userEntryToMessage(cur.Content, cur.Images, cur.Context.ToolResults))

	for _, t := range cur.Context.Tools {
		spec := t.ToolSpecification
		schema := spec.InputSchema.JSON
		if len(schema) == 0 {
			schema = json.RawMessage(`{}`)
		}
		req.Tools = append(req.Tools, ir.Tool{
			Name:        OriginalForToolName(spec.Name),
			Description: spec.Description,
			InputSchema: schema,
		})
	}
	return req, nil
}

// effortFromAdditionalFields 从 additionalModelRequestFields 取 effort 档位。
func effortFromAdditionalFields(fields map[string]struct {
	Effort string `json:"effort"`
}) string {
	for _, v := range fields {
		if v.Effort != "" {
			return v.Effort
		}
	}
	return ""
}

// decodeHistoryEntry history 单条：userInputMessage / assistantResponseMessage。
func decodeHistoryEntry(raw json.RawMessage) (*ir.Message, error) {
	var probe struct {
		UserInputMessage  *json.RawMessage `json:"userInputMessage"`
		AssistantResponse *json.RawMessage `json:"assistantResponseMessage"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("kiro decode history: %w", err)
	}
	switch {
	case probe.UserInputMessage != nil:
		var u struct {
			Content string          `json:"content"`
			Images  []kiroImageJSON `json:"images"`
			Context struct {
				ToolResults []kiroToolResultJSON `json:"toolResults"`
			} `json:"userInputMessageContext"`
		}
		if err := json.Unmarshal(*probe.UserInputMessage, &u); err != nil {
			return nil, fmt.Errorf("kiro decode history user: %w", err)
		}
		m := userEntryToMessage(u.Content, u.Images, u.Context.ToolResults)
		return &m, nil
	case probe.AssistantResponse != nil:
		var a struct {
			Content  string            `json:"content"`
			ToolUses []kiroToolUseJSON `json:"toolUses"`
		}
		if err := json.Unmarshal(*probe.AssistantResponse, &a); err != nil {
			return nil, fmt.Errorf("kiro decode history assistant: %w", err)
		}
		m := ir.Message{Role: ir.RoleAssistant}
		if a.Content != "" {
			m.Content = append(m.Content, ir.Block{Type: ir.BlockText, Text: a.Content})
		}
		for _, tu := range a.ToolUses {
			input := tu.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			m.Content = append(m.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:    tu.ToolUseID,
				Name:  OriginalForToolName(tu.Name),
				Input: input,
			}})
		}
		if len(m.Content) == 0 {
			m.Content = append(m.Content, ir.Block{Type: ir.BlockText, Text: ""})
		}
		return &m, nil
	default:
		return nil, fmt.Errorf("kiro decode history: unknown entry shape")
	}
}

// userEntryToMessage Kiro user 条目 -> IR user 消息（text + images + toolResults 块）。
func userEntryToMessage(content string, images []kiroImageJSON, toolResults []kiroToolResultJSON) ir.Message {
	m := ir.Message{Role: ir.RoleUser}
	if content != "" {
		m.Content = append(m.Content, ir.Block{Type: ir.BlockText, Text: content})
	}
	for _, img := range images {
		if img.Source.Bytes == "" {
			continue
		}
		mediaType := img.Format
		if mediaType == "" {
			mediaType = "image/png"
		}
		m.Content = append(m.Content, ir.Block{Type: ir.BlockImage, Image: &ir.Image{
			MediaType: mediaType,
			Data:      img.Source.Bytes,
		}})
	}
	for _, tr := range toolResults {
		var blocks []ir.Block
		for _, c := range tr.Content {
			blocks = append(blocks, ir.Block{Type: ir.BlockText, Text: c.Text})
		}
		if len(blocks) == 0 {
			blocks = append(blocks, ir.Block{Type: ir.BlockText, Text: ""})
		}
		m.Content = append(m.Content, ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
			ToolUseID: tr.ToolUseID,
			Content:   blocks,
			IsError:   tr.Status == "error",
		}})
	}
	return m
}
