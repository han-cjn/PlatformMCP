package semantic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// fakeCaller 记录调用参数,便于断言映射结果;可注入错误。
type fakeCaller struct {
	platform string
	tool     string
	args     map[string]any
	calls    int
	err      error
}

func (f *fakeCaller) CallTool(_ context.Context, platform, tool string, args map[string]any) (string, error) {
	f.calls++
	f.platform = platform
	f.tool = tool
	f.args = args
	if f.err != nil {
		return "", f.err
	}
	return "ok", nil
}

func TestBuiltinCatalogHasImSendText(t *testing.T) {
	cat := BuiltinCatalog()
	cap, ok := cat.Get("im.send_text")
	if !ok {
		t.Fatal("缺少内置能力 im.send_text")
	}
	if cap.Title == "" || cap.Description == "" {
		t.Fatal("能力应带 Title/Description")
	}
	supported := cap.SupportedPlatforms()
	want := []string{"dingtalk", "feishu", "wecom"}
	if !reflect.DeepEqual(supported, want) {
		t.Fatalf("支持平台应为 %v,实际 %v(腾讯文档应被 Unsupported 排除)", want, supported)
	}
}

// TestSendTextFeishuMapping 飞书映射必须与改造前 switch 分支逐字段等价。
func TestSendTextFeishuMapping(t *testing.T) {
	f := &fakeCaller{}
	res, err := Invoke(context.Background(), BuiltinCatalog(), f, "im.send_text", "feishu",
		map[string]any{"target": "oc_123", "text": "你好"}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.tool != "im_v1_message_create" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	if res.Platform != "feishu" || res.Capability != "im.send_text" {
		t.Fatalf("结果元信息不符: %+v", res)
	}

	// 与旧实现对比:params.receive_id_type=chat_id, data.receive_id, data.msg_type=text,
	// data.content 为 {"text":"你好"} 的 JSON 字符串
	params, _ := f.args["params"].(map[string]any)
	if params["receive_id_type"] != "chat_id" {
		t.Fatalf("params.receive_id_type 应为 chat_id,实际 %v", params["receive_id_type"])
	}
	data, _ := f.args["data"].(map[string]any)
	if data["receive_id"] != "oc_123" || data["msg_type"] != "text" {
		t.Fatalf("data 字段不符: %v", data)
	}
	content, _ := data["content"].(string)
	var decoded map[string]string
	if err := json.Unmarshal([]byte(content), &decoded); err != nil {
		t.Fatalf("content 应为 JSON 字符串: %v (%q)", err, content)
	}
	if decoded["text"] != "你好" {
		t.Fatalf("content.text 不符: %v", decoded)
	}
}

func TestSendTextDingTalkMapping(t *testing.T) {
	f := &fakeCaller{}
	if _, err := Invoke(context.Background(), BuiltinCatalog(), f, "im.send_text", "dingtalk",
		map[string]any{"target": "cid_abc", "text": "hello"}, nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.tool != "sendMessageToGroupByRobot" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	if f.args["openConversationId"] != "cid_abc" {
		t.Fatalf("openConversationId 不符: %v", f.args["openConversationId"])
	}
	// title 未提供:应回退默认值 "MCP Hub"(与旧实现一致)
	raw, _ := f.args["msgParam"].(string)
	var mp map[string]string
	if err := json.Unmarshal([]byte(raw), &mp); err != nil {
		t.Fatalf("msgParam 应为 JSON 字符串: %v (%q)", err, raw)
	}
	if mp["title"] != "MCP Hub" || mp["text"] != "hello" {
		t.Fatalf("msgParam 不符: %v", mp)
	}
}

func TestSendTextWeComMapping(t *testing.T) {
	f := &fakeCaller{}
	if _, err := Invoke(context.Background(), BuiltinCatalog(), f, "im.send_text", "wecom",
		map[string]any{"target": "chat1", "text": "hi"}, nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.tool != "wecom_send_text" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	if f.args["chatid"] != "chat1" || f.args["content"] != "hi" {
		t.Fatalf("企微参数不符: %v", f.args)
	}
}

// TestUnsupportedPlatform 腾讯文档对 im.send_text 明确不支持,错误应可读。
func TestUnsupportedPlatform(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "im.send_text", "tencent-docs",
		map[string]any{"target": "x", "text": "y"}, nil)
	if err == nil {
		t.Fatal("腾讯文档应报不支持")
	}
	if !strings.Contains(err.Error(), "不可用") || !strings.Contains(err.Error(), "无 IM") {
		t.Fatalf("错误信息应说明原因: %v", err)
	}
	if f.calls != 0 {
		t.Fatal("不支持时不应调用下游")
	}
}

// TestAutoPickPlatform platform 为空时按 preferred 顺序自动选择。
func TestAutoPickPlatform(t *testing.T) {
	f := &fakeCaller{}
	res, err := Invoke(context.Background(), BuiltinCatalog(), f, "im.send_text", "",
		map[string]any{"target": "t", "text": "x"}, []string{"wecom", "feishu"})
	if err != nil {
		t.Fatalf("自动选平台失败: %v", err)
	}
	if res.Platform != "wecom" {
		t.Fatalf("应优先选 wecom,实际 %s", res.Platform)
	}

	// preferred 全部不可用时回落到字典序第一个
	f2 := &fakeCaller{}
	res2, err := Invoke(context.Background(), BuiltinCatalog(), f2, "im.send_text", "",
		map[string]any{"target": "t", "text": "x"}, []string{"tencent-docs"})
	if err != nil {
		t.Fatalf("回落选平台失败: %v", err)
	}
	if res2.Platform != "dingtalk" {
		t.Fatalf("字典序首个应为 dingtalk,实际 %s", res2.Platform)
	}
}

func TestValidateRequired(t *testing.T) {
	cap := ImSendText
	// 缺 text
	if _, err := cap.Validate(map[string]any{"target": "t"}); err == nil {
		t.Fatal("缺 text 应报错")
	}
	// text 为空白
	if _, err := cap.Validate(map[string]any{"target": "t", "text": "   "}); err == nil {
		t.Fatal("空白 text 应报错")
	}
	// 类型错误
	if _, err := cap.Validate(map[string]any{"target": "t", "text": 123}); err == nil {
		t.Fatal("text 非字符串应报错")
	}
	// 正常 + 默认值补入
	out, err := cap.Validate(map[string]any{"target": "t", "text": "x"})
	if err != nil {
		t.Fatalf("正常入参不应报错: %v", err)
	}
	if out["title"] != "" {
		t.Fatalf("title 应补默认空串,实际 %v", out["title"])
	}
}

func TestValidateEnum(t *testing.T) {
	cap := &Capability{
		Name:   "test.enum",
		Schema: map[string]FieldSpec{"mode": {Type: "string", Enum: []string{"a", "b"}}},
	}
	if _, err := cap.Validate(map[string]any{"mode": "c"}); err == nil {
		t.Fatal("枚举外取值应报错")
	}
	if _, err := cap.Validate(map[string]any{"mode": "a"}); err != nil {
		t.Fatalf("枚举内取值不应报错: %v", err)
	}
}

func TestUnknownCapability(t *testing.T) {
	_, err := Invoke(context.Background(), BuiltinCatalog(), &fakeCaller{}, "no.such", "feishu", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "未知能力") {
		t.Fatalf("未知能力应报错: %v", err)
	}
}

// TestMappingExpr 表达式求值:字段引用 / 字面量 / ?? 回退。
func TestMappingExpr(t *testing.T) {
	domain := map[string]any{"a": "va", "empty": "", "n": 7}
	cases := []struct {
		expr string
		want any
	}{
		{"$.a", "va"},
		{"$.missing", nil},
		{"$const.hello", "hello"},
		{"$const.42", int64(42)},
		{"$const.true", true},
		{"$.empty ?? fallback", "fallback"},
		{"$.a ?? fallback", "va"},
		{"$.missing ?? plain", "plain"},
	}
	for _, c := range cases {
		got, err := evalExpr(c.expr, domain)
		if err != nil {
			t.Errorf("evalExpr(%q) 报错: %v", c.expr, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("evalExpr(%q) = %#v, want %#v", c.expr, got, c.want)
		}
	}
}

func TestSetPathNested(t *testing.T) {
	m := map[string]any{}
	setPath(m, "a.b.c", 1)
	setPath(m, "a.b.d", 2)
	setPath(m, "x", 3)
	a, _ := m["a"].(map[string]any)
	b, _ := a["b"].(map[string]any)
	if b["c"] != 1 || b["d"] != 2 || m["x"] != 3 {
		t.Fatalf("嵌套写入失败: %#v", m)
	}
}

// TestMatrixReport 矩阵应能区分 支持/未挂载/未声明/不支持。
func TestMatrixReport(t *testing.T) {
	cat := BuiltinCatalog()
	report := MatrixReport(cat, []string{"feishu", "dingtalk", "wecom", "tencent-docs"}, []string{"feishu"})
	for _, want := range []string{"im.send_text", "feishu", "tencent-docs", "图例"} {
		if !strings.Contains(report, want) {
			t.Fatalf("矩阵应包含 %q:\n%s", want, report)
		}
	}
	// 飞书已挂载 → ✓;钉钉支持未挂载 → ○;腾讯文档明确不支持 → ✗
	if !strings.Contains(report, "✓") || !strings.Contains(report, "○") || !strings.Contains(report, "✗") {
		t.Fatalf("矩阵符号不全:\n%s", report)
	}
}

// TestSendTextCompat SendText 的内部实现仍按旧行为工作(3 个既有调用点依赖它)。
func TestSendTextCompat(t *testing.T) {
	f := &fakeCaller{}
	text, err := sendTextVia(context.Background(), f, "feishu", "oc_1", "hi")
	if err != nil {
		t.Fatalf("SendText 失败: %v", err)
	}
	if text != "ok" || f.tool != "im_v1_message_create" {
		t.Fatalf("SendText 结果不符: text=%q tool=%s", text, f.tool)
	}
}

func TestAdapterMissing(t *testing.T) {
	err := errAdapterMissing("nope")
	if err == nil || !strings.Contains(err.Error(), "未注册") {
		t.Fatalf("缺失适配器错误不符: %v", err)
	}
}

// ---- format 参数与 markdown 支持 ----

// TestSendTextPlainDefault format 缺省为 plain,飞书走 text 类型。
func TestSendTextPlainDefault(t *testing.T) {
	f := &fakeCaller{}
	if _, err := Invoke(context.Background(), BuiltinCatalog(), f, "im.send_text", "feishu",
		map[string]any{"target": "oc_1", "text": "hello"}, nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	data, _ := f.args["data"].(map[string]any)
	if data["msg_type"] != "text" {
		t.Fatalf("默认应为 text 类型,实际 %v", data["msg_type"])
	}
	var dec map[string]string
	if err := json.Unmarshal([]byte(data["content"].(string)), &dec); err != nil {
		t.Fatalf("content 应为 JSON: %v", err)
	}
	if dec["text"] != "hello" {
		t.Fatalf("text 不符: %v", dec)
	}
}

// TestSendTextMarkdownFeishuUsesPost format=markdown 时飞书必须切 post 并生成嵌套结构。
func TestSendTextMarkdownFeishuUsesPost(t *testing.T) {
	f := &fakeCaller{}
	md := "# 日报\n\n**重点** 完成了 `重构`\n\n- 条目一\n- 条目二"
	if _, err := Invoke(context.Background(), BuiltinCatalog(), f, "im.send_text", "feishu",
		map[string]any{"target": "oc_1", "text": md, "format": "markdown"}, nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	data, _ := f.args["data"].(map[string]any)
	if data["msg_type"] != "post" {
		t.Fatalf("markdown 应使用 post 类型,实际 %v", data["msg_type"])
	}
	var post struct {
		ZhCn struct {
			Title   string `json:"title"`
			Content [][]struct {
				Tag   string   `json:"tag"`
				Text  string   `json:"text"`
				Style []string `json:"style"`
			} `json:"content"`
		} `json:"zh_cn"`
	}
	if err := json.Unmarshal([]byte(data["content"].(string)), &post); err != nil {
		t.Fatalf("post content 应为合法 JSON: %v", err)
	}
	// 首个 # 标题应被提取为 title
	if post.ZhCn.Title != "日报" {
		t.Fatalf("title 应取自首个标题,实际 %q", post.ZhCn.Title)
	}
	// 应有多行(标题/正文/两个列表项)
	if len(post.ZhCn.Content) < 4 {
		t.Fatalf("应生成至少 4 行,实际 %d: %+v", len(post.ZhCn.Content), post.ZhCn.Content)
	}
	// 列表项应带 • 前缀
	joined := ""
	for _, line := range post.ZhCn.Content {
		for _, in := range line {
			joined += in.Text + "\n"
		}
	}
	if !strings.Contains(joined, "• 条目一") {
		t.Fatalf("无序列表应转为 • 前缀:\n%s", joined)
	}
	if !strings.Contains(joined, "重构") {
		t.Fatalf("行内代码文本应保留:\n%s", joined)
	}
}

// TestSendTextMarkdownDingTalkKeepsRaw 钉钉原生支持 markdown,应原样透传(不转换)。
func TestSendTextMarkdownDingTalkKeepsRaw(t *testing.T) {
	f := &fakeCaller{}
	md := "**粗体** 内容"
	if _, err := Invoke(context.Background(), BuiltinCatalog(), f, "im.send_text", "dingtalk",
		map[string]any{"target": "cid", "text": md, "format": "markdown"}, nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	var mp map[string]string
	if err := json.Unmarshal([]byte(f.args["msgParam"].(string)), &mp); err != nil {
		t.Fatalf("msgParam 应为 JSON: %v", err)
	}
	if mp["text"] != md {
		t.Fatalf("钉钉应原样传递 markdown,实际 %q", mp["text"])
	}
}

// TestSendTextMarkdownWeComKeepsRaw 企微内部即 msgtype=markdown,content 原样传。
func TestSendTextMarkdownWeComKeepsRaw(t *testing.T) {
	f := &fakeCaller{}
	md := "- 列表\n- 项目"
	if _, err := Invoke(context.Background(), BuiltinCatalog(), f, "im.send_text", "wecom",
		map[string]any{"target": "chat", "text": md, "format": "markdown"}, nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.args["content"] != md {
		t.Fatalf("企微应原样传递 markdown,实际 %q", f.args["content"])
	}
}

// TestFormatEnumRejected 非法 format 应被枚举校验拦下。
func TestFormatEnumRejected(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "im.send_text", "feishu",
		map[string]any{"target": "t", "text": "x", "format": "html"}, nil)
	if err == nil || !strings.Contains(err.Error(), "不在允许范围") {
		t.Fatalf("非法 format 应报错: %v", err)
	}
	if f.calls != 0 {
		t.Fatal("校验失败不应调用下游")
	}
}

// ---- markdown → 飞书 post 转换器 ----

func TestMarkdownToFeishuPostBasics(t *testing.T) {
	out, err := MarkdownToFeishuPost("# 标题A\n\n普通段落", "")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		ZhCn struct {
			Title   string `json:"title"`
			Content [][]struct {
				Tag  string `json:"tag"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"zh_cn"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("输出非合法 JSON: %v\n%s", err, out)
	}
	if p.ZhCn.Title != "标题A" {
		t.Fatalf("title 不符: %q", p.ZhCn.Title)
	}
	if len(p.ZhCn.Content) != 2 {
		t.Fatalf("应两行,实际 %d", len(p.ZhCn.Content))
	}
}

func TestMarkdownToFeishuPostEmpty(t *testing.T) {
	out, err := MarkdownToFeishuPost("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"content"`) {
		t.Fatalf("空输入也应有 content 字段: %s", out)
	}
}

func TestMarkdownToFeishuPostStyles(t *testing.T) {
	out, err := MarkdownToFeishuPost("**粗** *斜* `码` ~~删~~", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"bold"`, `"italic"`, `"code"`, `"lineThrough"`} {
		if !strings.Contains(out, want) {
			t.Errorf("应包含样式 %s:\n%s", want, out)
		}
	}
}

func TestMarkdownToFeishuPostLink(t *testing.T) {
	out, err := MarkdownToFeishuPost("点击 [文档](https://example.com) 查看", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"tag":"a"`) || !strings.Contains(out, `"href":"https://example.com"`) {
		t.Fatalf("链接应转为 a 标签:\n%s", out)
	}
}

func TestMarkdownToFeishuPostOrderedList(t *testing.T) {
	out, err := MarkdownToFeishuPost("1. 第一\n2. 第二", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1. 第一") || !strings.Contains(out, "2. 第二") {
		t.Fatalf("有序列表应保留序号:\n%s", out)
	}
}

// TestMarkdownNoLeftoverMarkers 转换后不应残留 markdown 标记符(降级为纯文本)。
func TestMarkdownNoLeftoverMarkers(t *testing.T) {
	out, err := MarkdownToFeishuPost("**加粗**", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, `**`) {
		t.Fatalf("不应残留 ** 标记:\n%s", out)
	}
}

// ---- 文档能力 ----

// TestDocCreateTencentBase64 腾讯建文档必须把 markdown 编码为 base64_markdown。
func TestDocCreateTencentBase64(t *testing.T) {
	f := &fakeCaller{}
	res, err := Invoke(context.Background(), BuiltinCatalog(), f, "doc.create", "tencent-docs",
		map[string]any{"title": "周报", "markdown": "# 标题\n\n内容"}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.tool != "doc.create_with_markdown" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	if f.args["title"] != "周报" {
		t.Fatalf("title 应透传,实际 %v", f.args["title"])
	}
	// base64_markdown 解码后应还原为原始 markdown
	raw, _ := f.args["base64_markdown"].(string)
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("base64_markdown 解码失败: %v", err)
	}
	if string(decoded) != "# 标题\n\n内容" {
		t.Fatalf("解码内容不符: %q", string(decoded))
	}
	if res.Platform != "tencent-docs" || res.Capability != "doc.create" {
		t.Fatalf("结果元信息不符: %+v", res)
	}
}

// TestDocCreateFeishuImport 飞书用 docx_builtin_import,markdown 直接传不编码。
func TestDocCreateFeishuImport(t *testing.T) {
	f := &fakeCaller{}
	if _, err := Invoke(context.Background(), BuiltinCatalog(), f, "doc.create", "feishu",
		map[string]any{"title": "需求文档", "markdown": "## 概述"}, nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.tool != "docx_builtin_import" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	data, _ := f.args["data"].(map[string]any)
	if data["file_name"] != "需求文档" || data["markdown"] != "## 概述" {
		t.Fatalf("飞书参数不符: %v", data)
	}
}

// TestDocCreateUnsupportedOnIM 钉钉/企微应明确不支持 doc.create。
func TestDocCreateUnsupportedOnIM(t *testing.T) {
	for _, pl := range []string{"dingtalk", "wecom"} {
		f := &fakeCaller{}
		_, err := Invoke(context.Background(), BuiltinCatalog(), f, "doc.create", pl,
			map[string]any{"title": "t", "markdown": "x"}, nil)
		if err == nil || !strings.Contains(err.Error(), "不可用") {
			t.Fatalf("%s 应报不支持: %v", pl, err)
		}
		if f.calls != 0 {
			t.Fatalf("%s 不应调用下游", pl)
		}
	}
}

// TestDocAppendTencent 腾讯追加走 insert_markdown,file_id/idx/markdown 齐全。
func TestDocAppendTencent(t *testing.T) {
	f := &fakeCaller{}
	if _, err := Invoke(context.Background(), BuiltinCatalog(), f, "doc.append", "tencent-docs",
		map[string]any{"doc_id": "file_x", "markdown": "追加内容", "position": 0}, nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.tool != "doc.insert_markdown" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	if f.args["file_id"] != "file_x" || f.args["markdown"] != "追加内容" {
		t.Fatalf("参数不符: %v", f.args)
	}
}

// TestDocAppendFeishuUnsupported 飞书不支持追加,必须显式报错。
func TestDocAppendFeishuUnsupported(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "doc.append", "feishu",
		map[string]any{"doc_id": "x", "markdown": "y"}, nil)
	if err == nil || !strings.Contains(err.Error(), "暂无追加") {
		t.Fatalf("飞书应报不支持追加: %v", err)
	}
}

// TestDocSearch 两平台搜索都应映射到各自参数。
func TestDocSearch(t *testing.T) {
	f := &fakeCaller{}
	if _, err := Invoke(context.Background(), BuiltinCatalog(), f, "doc.search", "tencent-docs",
		map[string]any{"keyword": "周报"}, nil); err != nil {
		t.Fatalf("腾讯搜索失败: %v", err)
	}
	if f.args["search_key"] != "周报" {
		t.Fatalf("腾讯 search_key 不符: %v", f.args)
	}

	f2 := &fakeCaller{}
	if _, err := Invoke(context.Background(), BuiltinCatalog(), f2, "doc.search", "feishu",
		map[string]any{"keyword": "需求"}, nil); err != nil {
		t.Fatalf("飞书搜索失败: %v", err)
	}
	data, _ := f2.args["data"].(map[string]any)
	if data["search_key"] != "需求" {
		t.Fatalf("飞书 search_key 不符: %v", f2.args)
	}
}

// TestCapabilityMatrixNowHasDoc 矩阵应包含文档能力行。
func TestCapabilityMatrixNowHasDoc(t *testing.T) {
	cat := BuiltinCatalog()
	names := cat.Names()
	for _, want := range []string{"doc.append", "doc.create", "doc.search", "im.send_text", "table.query"} {
		if !contains(names, want) {
			t.Fatalf("能力目录缺少 %s,实际 %v", want, names)
		}
	}
	report := MatrixReport(cat, []string{"feishu", "dingtalk", "wecom", "tencent-docs"}, []string{"feishu"})
	for _, want := range []string{"doc.create", "doc.append", "doc.search", "table.query"} {
		if !strings.Contains(report, want) {
			t.Fatalf("矩阵缺少 %s:\n%s", want, report)
		}
	}
}

// ---- table.query ----

// TestTranslateOp 操作符映射:飞书 is/contains,钉钉 equal/contain。
func TestTranslateOp(t *testing.T) {
	cases := []struct {
		platform, op, want string
	}{
		{"feishu", "eq", "is"},
		{"feishu", "contains", "contains"},
		{"feishu", "gt", "isGreater"},
		{"feishu", "lte", "isLessEqual"},
		{"dingtalk", "eq", "equal"},
		{"dingtalk", "contains", "contain"},
		{"dingtalk", "gt", "greater"},
		{"dingtalk", "empty", "empty"},
	}
	for _, c := range cases {
		got, err := TranslateOp(c.platform, c.op)
		if err != nil || got != c.want {
			t.Errorf("TranslateOp(%s, %s) = %q, %v; want %q", c.platform, c.op, got, err, c.want)
		}
	}
	// 未知操作符应报错
	if _, err := TranslateOp("feishu", "regex"); err == nil {
		t.Error("未知操作符应报错")
	}
	// 未知平台应报错
	if _, err := TranslateOp("slack", "eq"); err == nil {
		t.Error("未知平台应报错")
	}
}

// TestTableQueryFeishu 飞书表格查询:path + filter.conjunction + field_name。
func TestTableQueryFeishu(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.query", "feishu",
		map[string]any{
			"app":   "bascnABC",
			"table": "tblXYZ",
			"filters": []any{
				map[string]any{"field": "状态", "op": "eq", "value": "进行中"},
				map[string]any{"field": "优先级", "op": "gte", "value": "3"},
			},
			"conjunction": "and",
			"page_size":   200,
		}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.tool != "bitable_v1_appTableRecord_search" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	path, _ := f.args["path"].(map[string]any)
	if path["app_token"] != "bascnABC" || path["table_id"] != "tblXYZ" {
		t.Fatalf("path 不符: %v", path)
	}
	data, _ := f.args["data"].(map[string]any)
	filter, _ := data["filter"].(map[string]any)
	if filter["conjunction"] != "and" {
		t.Fatalf("conjunction 不符: %v", filter)
	}
	conds, _ := filter["conditions"].([]map[string]any)
	if len(conds) != 2 {
		t.Fatalf("应有 2 个条件,实际 %d", len(conds))
	}
	if conds[0]["field_name"] != "状态" || conds[0]["operator"] != "is" {
		t.Fatalf("第一条条件不符: %v", conds[0])
	}
	if conds[1]["operator"] != "isGreaterEqual" {
		t.Fatalf("gte 应翻译为 isGreaterEqual,实际 %v", conds[1]["operator"])
	}
	params, _ := f.args["params"].(map[string]any)
	if params["page_size"] != 200 {
		t.Fatalf("page_size 不符: %v", params)
	}
}

// TestTableQueryDingTalk 钉钉表格查询:operatorId 必填,filter.combination + field。
func TestTableQueryDingTalk(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.query", "dingtalk",
		map[string]any{
			"app":         "baseABC",
			"table":       "sheet1",
			"operator_id": "union_123",
			"filters":     []any{map[string]any{"field": "负责人", "op": "contains", "value": "张三"}},
			"conjunction": "or",
		}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.tool != "listNotableRecords" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	if f.args["operatorId"] != "union_123" {
		t.Fatalf("operatorId 不符: %v", f.args["operatorId"])
	}
	if f.args["baseId"] != "baseABC" || f.args["sheetIdOrName"] != "sheet1" {
		t.Fatalf("定位参数不符: %v", f.args)
	}
	filter, _ := f.args["filter"].(map[string]any)
	if filter["combination"] != "or" {
		t.Fatalf("combination 不符: %v", filter)
	}
	conds, _ := filter["conditions"].([]map[string]any)
	if len(conds) != 1 || conds[0]["field"] != "负责人" || conds[0]["operator"] != "contain" {
		t.Fatalf("条件不符: %v", conds)
	}
}

// TestTableQueryDingTalkMissingOperator 钉钉缺 operator_id 应报错。
func TestTableQueryDingTalkMissingOperator(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.query", "dingtalk",
		map[string]any{"app": "base", "table": "sheet"}, nil)
	if err == nil || !strings.Contains(err.Error(), "operator_id") {
		t.Fatalf("缺 operator_id 应报错: %v", err)
	}
	if f.calls != 0 {
		t.Fatal("校验失败不应调用下游")
	}
}

// TestTableQueryDefaultConjunction conjunction 缺省应为 and。
func TestTableQueryDefaultConjunction(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.query", "feishu",
		map[string]any{"app": "a", "table": "t"}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	data, _ := f.args["data"].(map[string]any)
	// 无 filters 时不应生成 filter 键
	if _, hasFilter := data["filter"]; hasFilter {
		t.Fatalf("无 filters 时不应有 filter: %v", data)
	}
}

// TestTableQueryUnsupported 其他平台应显式不支持。
func TestTableQueryUnsupported(t *testing.T) {
	for _, pl := range []string{"wecom", "tencent-docs"} {
		f := &fakeCaller{}
		_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.query", pl,
			map[string]any{"app": "a", "table": "t"}, nil)
		if err == nil || !strings.Contains(err.Error(), "不可用") {
			t.Fatalf("%s 应报不支持: %v", pl, err)
		}
	}
}

// TestTableQueryNoValueOps isEmpty/isNotEmpty 在飞书必须传空数组 value:[]。
// 官方文档:当 value 填空值 [] 时,请确保按照 value:[] 格式传入,否则报缺失 value 错误。
func TestTableQueryNoValueOps(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.query", "feishu",
		map[string]any{
			"app":     "a",
			"table":   "t",
			"filters": []any{map[string]any{"field": "内容", "op": "not_empty", "value": ""}},
		}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	data, _ := f.args["data"].(map[string]any)
	filter, _ := data["filter"].(map[string]any)
	conds, _ := filter["conditions"].([]map[string]any)
	if len(conds) != 1 {
		t.Fatalf("应有 1 个条件,实际 %d", len(conds))
	}
	if conds[0]["operator"] != "isNotEmpty" {
		t.Fatalf("not_empty 应翻译为 isNotEmpty,实际 %v", conds[0]["operator"])
	}
	// 飞书:value 必须为 [] 空数组(不是 [""] 也不是省略)
	value, hasValue := conds[0]["value"].([]string)
	if !hasValue {
		t.Fatalf("飞书 isNotEmpty 必须传 value: %v", conds[0])
	}
	if len(value) != 0 {
		t.Fatalf("value 应为空数组 [],实际 %v", value)
	}
}

// TestOpNeedsValue 操作符 value 需求判定。
func TestOpNeedsValue(t *testing.T) {
	if OpNeedsValue("empty") || OpNeedsValue("not_empty") {
		t.Error("empty/not_empty 不应需要 value")
	}
	for _, op := range []string{"eq", "ne", "contains", "gt", "lt"} {
		if !OpNeedsValue(op) {
			t.Errorf("%s 应需要 value", op)
		}
	}
}

// TestParseFilters filters 解析与校验。
func TestParseFilters(t *testing.T) {
	// 正常
	filters, err := ParseFilters([]any{map[string]any{"field": "a", "op": "eq", "value": "v"}})
	if err != nil || len(filters) != 1 || filters[0].Field != "a" {
		t.Fatalf("解析失败: %v %v", filters, err)
	}
	// 缺 field 报错
	if _, err := ParseFilters([]any{map[string]any{"op": "eq"}}); err == nil {
		t.Error("缺 field 应报错")
	}
	// 未知 op
	if _, err := ParseFilters([]any{map[string]any{"field": "a", "op": "regex"}}); err == nil {
		t.Error("未知 op 应报错")
	}
	// 非数组
	if _, err := ParseFilters("not-array"); err == nil {
		t.Error("非数组应报错")
	}
	// nil 返回空
	if filters, err := ParseFilters(nil); err != nil || len(filters) != 0 {
		t.Errorf("nil 应返回空: %v %v", filters, err)
	}
}

// ---- table.create / table.update ----

// TestTableCreateFeishu 飞书单条创建:循环调用,验证 records 数组被拆开。
func TestTableCreateFeishu(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.create", "feishu",
		map[string]any{
			"app":   "bascnABC",
			"table": "tblXYZ",
			"records": []any{
				map[string]any{"fields": map[string]any{"内容": "第一条", "来源平台": "测试"}},
				map[string]any{"fields": map[string]any{"内容": "第二条", "来源平台": "测试"}},
			},
		}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.calls != 2 {
		t.Fatalf("飞书单条 API 应调用 2 次,实际 %d", f.calls)
	}
	if f.tool != "bitable_v1_appTableRecord_create" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	path, _ := f.args["path"].(map[string]any)
	if path["app_token"] != "bascnABC" || path["table_id"] != "tblXYZ" {
		t.Fatalf("path 不符: %v", path)
	}
	data, _ := f.args["data"].(map[string]any)
	fields, _ := data["fields"].(map[string]any)
	if fields["内容"] == nil {
		t.Fatalf("fields 不符: %v", data)
	}
}

// TestTableCreateFeishuIdempotency 幂等键应传入 params.client_token。
func TestTableCreateFeishuIdempotency(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.create", "feishu",
		map[string]any{
			"app":             "a",
			"table":           "t",
			"records":         []any{map[string]any{"fields": map[string]any{"k": "v"}}},
			"idempotency_key": "uuid-123",
		}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	params, _ := f.args["params"].(map[string]any)
	if params["client_token"] != "uuid-123" {
		t.Fatalf("client_token 不符: %v", params)
	}
}

// TestTableCreateDingTalk 钉钉批量创建:一次调用,records 数组结构。
func TestTableCreateDingTalk(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.create", "dingtalk",
		map[string]any{
			"app":         "baseABC",
			"table":       "sheet1",
			"operator_id": "union_123",
			"records": []any{
				map[string]any{"fields": map[string]any{"标题": "A"}},
				map[string]any{"fields": map[string]any{"标题": "B"}},
			},
		}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("钉钉批量 API 应调用 1 次,实际 %d", f.calls)
	}
	if f.tool != "insertNotableRecords" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	if f.args["operatorId"] != "union_123" || f.args["baseId"] != "baseABC" || f.args["sheetIdOrName"] != "sheet1" {
		t.Fatalf("定位参数不符: %v", f.args)
	}
	rows, _ := f.args["records"].([]map[string]any)
	if len(rows) != 2 {
		t.Fatalf("应有 2 条记录,实际 %d", len(rows))
	}
	fields, _ := rows[0]["fields"].(map[string]any)
	if fields["标题"] != "A" {
		t.Fatalf("第一条记录不符: %v", rows[0])
	}
}

// TestTableCreateDingTalkMissingOperator 钉钉缺 operator_id 应报错。
func TestTableCreateDingTalkMissingOperator(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.create", "dingtalk",
		map[string]any{
			"app":     "a",
			"table":   "t",
			"records": []any{map[string]any{"fields": map[string]any{"k": "v"}}},
		}, nil)
	if err == nil || !strings.Contains(err.Error(), "operator_id") {
		t.Fatalf("缺 operator_id 应报错: %v", err)
	}
	if f.calls != 0 {
		t.Fatal("校验失败不应调用下游")
	}
}

// TestTableUpdateFeishu 飞书单条更新:record_id 在 path 里。
func TestTableUpdateFeishu(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.update", "feishu",
		map[string]any{
			"app":   "bascnABC",
			"table": "tblXYZ",
			"records": []any{
				map[string]any{"id": "rec_001", "fields": map[string]any{"状态": "已完成"}},
			},
		}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.tool != "bitable_v1_appTableRecord_update" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	path, _ := f.args["path"].(map[string]any)
	if path["record_id"] != "rec_001" {
		t.Fatalf("record_id 应在 path 里: %v", path)
	}
}

// TestTableUpdateDingTalk 钉钉批量更新:id 在 records[].id 里。
func TestTableUpdateDingTalk(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.update", "dingtalk",
		map[string]any{
			"app":         "baseABC",
			"table":       "sheet1",
			"operator_id": "union_123",
			"records": []any{
				map[string]any{"id": "row_001", "fields": map[string]any{"状态": "已完成"}},
				map[string]any{"id": "row_002", "fields": map[string]any{"状态": "进行中"}},
			},
		}, nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.tool != "updateNotableRecords" {
		t.Fatalf("下游工具不符: %s", f.tool)
	}
	rows, _ := f.args["records"].([]map[string]any)
	if len(rows) != 2 {
		t.Fatalf("应有 2 条记录,实际 %d", len(rows))
	}
	if rows[0]["id"] != "row_001" || rows[1]["id"] != "row_002" {
		t.Fatalf("记录 id 不符: %v", rows)
	}
}

// TestTableUpdateMissingID 更新缺少 id 应报错。
func TestTableUpdateMissingID(t *testing.T) {
	f := &fakeCaller{}
	_, err := Invoke(context.Background(), BuiltinCatalog(), f, "table.update", "feishu",
		map[string]any{
			"app":     "a",
			"table":   "t",
			"records": []any{map[string]any{"fields": map[string]any{"k": "v"}}},
		}, nil)
	if err == nil || !strings.Contains(err.Error(), "缺少 id") {
		t.Fatalf("缺 id 应报错: %v", err)
	}
}

// TestParseRecords records 解析与校验。
func TestParseRecords(t *testing.T) {
	// 正常
	records, err := parseRecords([]any{
		map[string]any{"id": "r1", "fields": map[string]any{"a": 1}},
	})
	if err != nil || len(records) != 1 || records[0].ID != "r1" {
		t.Fatalf("解析失败: %v %v", records, err)
	}
	// 缺 fields 报错
	if _, err := parseRecords([]any{map[string]any{"id": "r1"}}); err == nil {
		t.Error("缺 fields 应报错")
	}
	// 非数组
	if _, err := parseRecords("not-array"); err == nil {
		t.Error("非数组应报错")
	}
	// nil 报错
	if _, err := parseRecords(nil); err == nil {
		t.Error("nil 应报错")
	}
}

func TestParseLiteral(t *testing.T) {
	cases := map[string]any{
		"true": true, "false": false, "42": int64(42),
		"3.14": 3.14, "hello": "hello",
	}
	for in, want := range cases {
		if got := parseLiteral(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parseLiteral(%q) = %#v, want %#v", in, got, want)
		}
	}
}
