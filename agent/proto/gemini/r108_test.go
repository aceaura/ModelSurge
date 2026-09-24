package gemini

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R108-乙6 googleSearch.excludeDomains 与 IR 域名黑名单同义：映进
// HostedParams.BlockedDomains 后跨族直通，不再随声明形态蒸发。
func TestGeminiR108ExcludeDomainsMapped(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"tools":[{"googleSearch":{"excludeDomains":["amazon.com","facebook.com"]}}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	var found *ir.Tool
	for i := range req.Tools {
		if req.Tools[i].Hosted == ir.HostedWebSearch {
			found = &req.Tools[i]
		}
	}
	if found == nil || found.HostedParams == nil {
		t.Fatalf("hosted web search params = %+v", req.Tools)
	}
	if len(found.HostedParams.BlockedDomains) != 2 || found.HostedParams.BlockedDomains[0] != "amazon.com" {
		t.Fatalf("BlockedDomains = %v", found.HostedParams.BlockedDomains)
	}
}

// R108-乙6 computerUse/enterpriseWebSearch/parallelAiSearch/mcpServers 按
// urlContext 先例收为未识别托管条目：出站丢得可见，不再解码即蒸发。
func TestGeminiR108NewHostedFormsCollected(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[` +
		`{"computerUse":{"environment":"ENVIRONMENT_BROWSER"}},` +
		`{"enterpriseWebSearch":{}},` +
		`{"parallelAiSearch":{"apiKey":"k"}},` +
		`{"mcpServers":[{"name":"srv"}]}` +
		`]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tl := range req.Tools {
		if tl.Hosted != "" {
			got[tl.Hosted] = tl.HostedType
		}
	}
	for hosted, wireType := range map[string]string{
		"computer_use":          "computerUse",
		"enterprise_web_search": "enterpriseWebSearch",
		"parallel_ai_search":    "parallelAiSearch",
		"mcp_servers":           "mcpServers",
	} {
		if got[hosted] != wireType {
			t.Errorf("%s: HostedType = %q, want %q", hosted, got[hosted], wireType)
		}
	}
}
