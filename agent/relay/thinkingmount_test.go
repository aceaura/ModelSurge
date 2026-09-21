package relay

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// thinkingmount_test.go 源码级守卫：每条上游请求路径都必须调用
// ir.CompleteThinking。
//
// 为什么需要源码级守卫而不是行为断言：本包有四条互不相通的上游路径
// （attempt / attemptKiro / fetchSummary / fetchKiroSummary），各自 Clone
// 请求后独立编码，没有共同漏斗。漏掉任何一条都是静默的——请求照常成功，
// 只是那一维悄悄换成了出站协议自己的硬编码缺省。编译器管不住这种遗漏，
// 而按行为逐条覆盖需要为每条路径搭一套上游夹具（kiro 路径还要 replay mock）。
//
// 形态取自既有纪律：R18 的 transportOptions、R28 的 newListener 都是把
// 「装配」抽出来测；这里装配点无法收拢（四条路径的前后处理各不相同），
// 于是退一步用 AST 断言每条路径都调了那一行。

// upstreamRequestPaths 需要推理风格补全的函数名 -> 所在文件。
// 新增上游路径时必须同时在这里登记，否则本用例不会覆盖到它——这一点由
// TestNoUnregisteredUpstreamPath 兜住（它反向扫描所有 Clone 点）。
var upstreamRequestPaths = map[string]string{
	"attempt":          "forward.go",
	"attemptKiro":      "kiro_remote.go",
	"fetchSummary":     "autocompact.go",
	"fetchKiroSummary": "autocompact.go",
}

func TestEveryUpstreamPathCompletesThinking(t *testing.T) {
	for fn, file := range upstreamRequestPaths {
		t.Run(fn, func(t *testing.T) {
			body := funcBody(t, file, fn)
			if !callsCompleteThinking(body) {
				t.Fatalf("%s (%s) does not call ir.CompleteThinking: the reasoning style "+
					"the client did not state would be replaced by the outbound codec's "+
					"hardcoded default, silently and with HTTP 200", fn, file)
			}
		})
	}
}

// 反向守卫：本包里每个 req.Clone() 都发生在已登记的上游路径里。
// 新增一条上游路径而忘了登记时，这条会红——否则上面那个用例会因为
// 「没登记就不检查」而假通过。
func TestNoUnregisteredUpstreamPath(t *testing.T) {
	// 非上游路径的 Clone：这些不发上游请求，不需要补全。
	allowed := map[string]string{
		// tryAutoCompact 的 Clone 只是历史快照，供 SplitForCompact /
		// BuildCompactRequest / BuildCompactedHistory 重建用；它触发的上游调用
		// 走 runCompactCall -> fetchSummary / fetchKiroSummary，两者已登记。
		"tryAutoCompact":          "autocompact.go",
		"appendRecoveryDirective": "toolpolicy.go", // 在已补全的 upReq 上追加指令
	}
	for _, file := range []string{"forward.go", "kiro_remote.go", "autocompact.go", "toolpolicy.go"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				return true
			}
			if !clonesRequest(fd.Body) {
				return true
			}
			name := fd.Name.Name
			if upstreamRequestPaths[name] == file {
				return true
			}
			if allowed[name] == file {
				return true
			}
			t.Errorf("%s in %s clones the request but is registered neither as an "+
				"upstream path (which must call ir.CompleteThinking) nor as an exemption",
				name, file)
			return true
		})
	}
}

func funcBody(t *testing.T, file, fn string) *ast.BlockStmt {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var body *ast.BlockStmt
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if ok && fd.Name.Name == fn {
			body = fd.Body
			return false
		}
		return true
	})
	if body == nil {
		t.Fatalf("function %s not found in %s (renamed? then update upstreamRequestPaths)", fn, file)
	}
	return body
}

func callsCompleteThinking(body *ast.BlockStmt) bool {
	return hasCall(body, "ir", "CompleteThinking")
}

func clonesRequest(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Clone" {
			return true
		}
		// 只认 *ir.Request 的 Clone：形参名一律以 req 开头（req / upReq）。
		if id, ok := sel.X.(*ast.Ident); ok && strings.HasPrefix(strings.ToLower(id.Name), "req") {
			found = true
			return false
		}
		return true
	})
	return found
}

func hasCall(body *ast.BlockStmt, pkg, fn string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != fn {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
			found = true
			return false
		}
		return true
	})
	return found
}
