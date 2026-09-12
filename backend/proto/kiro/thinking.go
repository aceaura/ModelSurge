// thinking.go 流式响应中的思考块标签解析
// （KiroaaS thinking_parser.py 的 Go 翻译）。
// 有限状态机：仅在响应开头检测开标签（<thinking>/<think>/<reasoning>/<thought>），
// "谨慎发送"——缓冲潜在标签碎片避免跨 chunk 撕裂；闭标签后全部按正文处理。
package kiro

import (
	"strings"
)

// thinkingState FSM 状态。
type thinkingState int

const (
	preContent thinkingState = iota // 缓冲中，检测开标签
	inThinking                      // 思考块内，缓冲至闭标签
	streaming                       // 常规流，不再检测
)

// thinkingOpenTags 默认检测的开标签集（KiroaaS FAKE_REASONING_OPEN_TAGS）。
var thinkingOpenTags = []string{"<thinking>", "<think>", "<reasoning>", "<thought>"}

// thinkingInitialBufferSize 开标签检测缓冲上限（字符）。
const thinkingInitialBufferSize = 20

// thinkingParseResult 一次 feed 的产出。
type thinkingParseResult struct {
	thinkingContent string
	regularContent  string
	isFirstThinking bool
	isLastThinking  bool
	stateChanged    bool
}

// thinkingParser 思考块 FSM 解析器（fake_reasoning 时启用）。
type thinkingParser struct {
	state          thinkingState
	initialBuffer  string
	thinkingBuffer string
	openTag        string
	closeTag       string
	maxTagLength   int
	isFirstChunk   bool
	found          bool
}

func newThinkingParser() *thinkingParser {
	max := 0
	for _, t := range thinkingOpenTags {
		if len(t) > max {
			max = len(t)
		}
	}
	return &thinkingParser{
		state:        preContent,
		maxTagLength: max * 2,
		isFirstChunk: true,
	}
}

// feed 处理一块正文增量。
func (tp *thinkingParser) feed(content string) thinkingParseResult {
	if content == "" {
		return thinkingParseResult{}
	}
	var result thinkingParseResult
	if tp.state == preContent {
		result = tp.handlePreContent(content)
	}
	if tp.state == inThinking && !result.stateChanged {
		result = tp.handleInThinking(content)
	}
	if tp.state == streaming && !result.stateChanged {
		result.regularContent = content
	}
	return result
}

func (tp *thinkingParser) handlePreContent(content string) thinkingParseResult {
	var result thinkingParseResult
	tp.initialBuffer += content
	stripped := strings.TrimLeft(tp.initialBuffer, " \t\r\n")

	for _, tag := range thinkingOpenTags {
		if strings.HasPrefix(stripped, tag) {
			tp.state = inThinking
			tp.openTag = tag
			tp.closeTag = "</" + tag[1:]
			tp.found = true
			result.stateChanged = true
			tp.thinkingBuffer = stripped[len(tag):]
			tp.initialBuffer = ""
			tr := tp.processThinkingBuffer()
			if tr.thinkingContent != "" {
				result.thinkingContent = tr.thinkingContent
				result.isFirstThinking = tr.isFirstThinking
			}
			result.isLastThinking = tr.isLastThinking
			if tr.regularContent != "" {
				result.regularContent = tr.regularContent
			}
			return result
		}
	}

	// 可能还在收标签前缀：继续缓冲
	for _, tag := range thinkingOpenTags {
		if len(stripped) < len(tag) && strings.HasPrefix(tag, stripped) {
			return result
		}
	}

	// 超限或不可能成为标签前缀 -> 常规流
	if len(tp.initialBuffer) > thinkingInitialBufferSize || !couldBeTagPrefix(stripped) {
		tp.state = streaming
		result.stateChanged = true
		result.regularContent = tp.initialBuffer
		tp.initialBuffer = ""
	}
	return result
}

func couldBeTagPrefix(text string) bool {
	if text == "" {
		return true
	}
	for _, tag := range thinkingOpenTags {
		if strings.HasPrefix(tag, text) {
			return true
		}
	}
	return false
}

func (tp *thinkingParser) handleInThinking(content string) thinkingParseResult {
	tp.thinkingBuffer += content
	return tp.processThinkingBuffer()
}

// processThinkingBuffer 谨慎发送：缓冲尾部 maxTagLength 字符，
// 避免闭标签被跨 chunk 撕裂后当作思考内容发出。
func (tp *thinkingParser) processThinkingBuffer() thinkingParseResult {
	var result thinkingParseResult
	if tp.closeTag == "" {
		return result
	}
	if idx := strings.Index(tp.thinkingBuffer, tp.closeTag); idx != -1 {
		thinking := tp.thinkingBuffer[:idx]
		after := tp.thinkingBuffer[idx+len(tp.closeTag):]
		if thinking != "" {
			result.thinkingContent = thinking
			result.isFirstThinking = tp.isFirstChunk
			tp.isFirstChunk = false
		}
		result.isLastThinking = true
		tp.state = streaming
		result.stateChanged = true
		tp.thinkingBuffer = ""
		if stripped := strings.TrimLeft(after, " \t\r\n"); stripped != "" {
			result.regularContent = stripped
		}
		return result
	}
	if len(tp.thinkingBuffer) > tp.maxTagLength {
		send := tp.thinkingBuffer[:len(tp.thinkingBuffer)-tp.maxTagLength]
		tp.thinkingBuffer = tp.thinkingBuffer[len(tp.thinkingBuffer)-tp.maxTagLength:]
		result.thinkingContent = send
		result.isFirstThinking = tp.isFirstChunk
		tp.isFirstChunk = false
	}
	return result
}

// finalize 流结束时冲刷残余缓冲。
func (tp *thinkingParser) finalize() thinkingParseResult {
	var result thinkingParseResult
	if tp.thinkingBuffer != "" {
		if tp.state == inThinking {
			result.thinkingContent = tp.thinkingBuffer
			result.isFirstThinking = tp.isFirstChunk
			result.isLastThinking = true
		} else {
			result.regularContent = tp.thinkingBuffer
		}
		tp.thinkingBuffer = ""
	}
	if tp.initialBuffer != "" {
		result.regularContent += tp.initialBuffer
		tp.initialBuffer = ""
	}
	return result
}
