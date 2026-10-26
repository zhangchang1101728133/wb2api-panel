package panel

import (
	"os"
	"strings"
	"testing"
)

// testJS 前端测试用的「单文件包」：把 index.html 里加载的同源脚本按顺序拼起来。
//
// 为什么需要：前端逻辑原来都在 app.js 里，测试的 node harness 直接吃这个文件。
// 拆成多文件后，harness 仍然只接受一个文件，于是由 TestMain 生成拼接包，保证
// harness 看到的执行顺序与浏览器完全一致（顶层 let/const 的先后依赖不能乱）。
const testJS = "app.test.js"

// panelScripts 与 index.html 的 <script src> 顺序保持一致（不含 mobile.js：
// 它有自己的窄屏 harness）。
var panelScripts = []string{
	"core.js",
	"views-accounts.js",
	"views-config.js",
	"views-tasks.js",
	"views-usage.js",
	"app.js",
}

func TestMain(m *testing.M) {
	if err := buildTestJS(); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.Remove(testJS)
	os.Exit(code)
}

func buildTestJS() error {
	var b strings.Builder
	for _, name := range panelScripts {
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		b.Write(data)
		b.WriteString("\n")
	}
	return os.WriteFile(testJS, []byte(b.String()), 0o644)
}
