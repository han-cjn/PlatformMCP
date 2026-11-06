package semantic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// AdapterFunc 复杂场景的具名适配器:能拿到完整的领域入参,自行编排多步调用。
// 仅当声明式映射(Args/Const/Encode)无法表达时才需要,属于兜底手段。
//
// 约定:
//   - 返回下游文本结果
//   - 不自行做入参校验(引擎已校验)
//   - 只调用 caller.CallTool,不直接持有 *adapter.Manager,便于单测注入
type AdapterFunc func(ctx context.Context, caller Caller, platform, tool string, domain map[string]any) (string, error)

// adapters 具名适配器注册表。
//
//   - feishu.send_text: 飞书 markdown 需转 msg_type=post 富文本嵌套结构
//   - tencent.doc_create: base64_markdown 需编码
//   - feishu.table_query / dingtalk.table_query: 条件结构组装 + 操作符词汇翻译
//
// 钉钉/企微的文本接口原生支持 markdown,仍走声明式映射。
var adapters = map[string]AdapterFunc{
	"feishu.send_text":      feishuSendText,
	"tencent.doc_create":    tencentDocCreate,
	"feishu.table_query":    feishuTableQuery,
	"dingtalk.table_query":  dingtalkTableQuery,
	"feishu.table_create":   feishuTableCreate,
	"feishu.table_update":   feishuTableUpdate,
	"dingtalk.table_create": dingtalkTableCreate,
	"dingtalk.table_update": dingtalkTableUpdate,
}

// tencentDocCreate 腾讯文档建文档适配器:base64_markdown 需编码,无法用字段映射表达。
func tencentDocCreate(ctx context.Context, caller Caller, platform, tool string, domain map[string]any) (string, error) {
	title, _ := domain["title"].(string)
	markdown, _ := domain["markdown"].(string)
	return caller.CallTool(ctx, platform, tool, map[string]any{
		"title":           title,
		"base64_markdown": base64.StdEncoding.EncodeToString([]byte(markdown)),
	})
}

// feishuTableQuery 飞书多维表格查询:组装 path + data.filter 结构。
//
// 结构(2026-11 实测 schema):
//
//	path:  {app_token, table_id}
//	data:  {filter: {conjunction: and/or, conditions: [{field_name, operator, value:[...]}]},
//	        field_names: [...], automatic_fields: false}
//	params: {page_size}
func feishuTableQuery(ctx context.Context, caller Caller, platform, tool string, domain map[string]any) (string, error) {
	app, _ := domain["app"].(string)
	table, _ := domain["table"].(string)
	conj, _ := domain["conjunction"].(string)
	if conj == "" {
		conj = "and"
	}
	pageSize := numOrDefault(domain["page_size"], 100)

	filters, err := ParseFilters(domain["filters"])
	if err != nil {
		return "", err
	}
	conditions := make([]map[string]any, 0, len(filters))
	for _, f := range filters {
		op, err := TranslateOp("feishu", f.Op)
		if err != nil {
			return "", err
		}
		cond := map[string]any{
			"field_name": f.Field,
			"operator":   op,
		}
		// 飞书:value 必传。isEmpty/isNotEmpty 传空数组 [](官方文档明确:
		// "当 value 填空值 [] 时,请确保按照 value:[] 格式传入,否则报缺失 value 错误")
		if OpNeedsValue(f.Op) {
			cond["value"] = []string{f.Value}
		} else {
			cond["value"] = []string{}
		}
		conditions = append(conditions, cond)
	}

	data := map[string]any{}
	if len(conditions) > 0 {
		data["filter"] = map[string]any{"conjunction": conj, "conditions": conditions}
	}

	return caller.CallTool(ctx, platform, tool, map[string]any{
		"path":   map[string]any{"app_token": app, "table_id": table},
		"params": map[string]any{"page_size": pageSize},
		"data":   data,
	})
}

// dingtalkTableQuery 钉钉智能表格查询:需要 operatorId,filter 结构键名不同。
//
// 结构:
//
//	operatorId / baseId / sheetIdOrName / maxResults / nextToken
//	filter: {combination: and/or, conditions: [{field, operator, value:[...]}]}
func dingtalkTableQuery(ctx context.Context, caller Caller, platform, tool string, domain map[string]any) (string, error) {
	operatorID, _ := domain["operator_id"].(string)
	if operatorID == "" {
		return "", fmt.Errorf("table.query 在钉钉上必须提供 operator_id(操作人 unionId)")
	}
	baseID, _ := domain["app"].(string)
	sheet, _ := domain["table"].(string)
	conj, _ := domain["conjunction"].(string)
	if conj == "" {
		conj = "and"
	}
	maxResults := numOrDefault(domain["page_size"], 100)

	filters, err := ParseFilters(domain["filters"])
	if err != nil {
		return "", err
	}
	conditions := make([]map[string]any, 0, len(filters))
	for _, f := range filters {
		op, err := TranslateOp("dingtalk", f.Op)
		if err != nil {
			return "", err
		}
		cond := map[string]any{
			"field":    f.Field,
			"operator": op,
		}
		if OpNeedsValue(f.Op) {
			cond["value"] = []string{f.Value}
		}
		conditions = append(conditions, cond)
	}

	return caller.CallTool(ctx, platform, tool, map[string]any{
		"operatorId":    operatorID,
		"baseId":        baseID,
		"sheetIdOrName": sheet,
		"maxResults":    maxResults,
		"filter":        map[string]any{"combination": conj, "conditions": conditions},
	})
}

// numOrDefault 从领域入参取数字,缺失/非法时用默认值。
func numOrDefault(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return def
}

// ---- 表格写入 ----

// tableRecord 一条表格记录(领域模型)。
type tableRecord struct {
	ID     string         `json:"id"`     // 更新时的记录 ID(创建时为空)
	Fields map[string]any `json:"fields"` // 字段名 -> 值
}

// parseRecords 把领域入参的 records(任意形态)解析为强类型记录列表。
func parseRecords(raw any) ([]tableRecord, error) {
	if raw == nil {
		return nil, fmt.Errorf("records 不能为空")
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("records 应为数组,实际 %T", raw)
	}
	out := make([]tableRecord, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("records[%d] 应为对象,实际 %T", i, item)
		}
		rec := tableRecord{}
		if id, ok := m["id"].(string); ok {
			rec.ID = id
		}
		fields, ok := m["fields"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("records[%d] 缺少 fields 对象", i)
		}
		rec.Fields = fields
		out = append(out, rec)
	}
	return out, nil
}

// feishuTableCreate 飞书单条创建:循环调用 bitable_v1_appTableRecord_create。
// 幂等:idempotency_key 存在时传入 params.client_token(同键重复调用不产生重复记录)。
func feishuTableCreate(ctx context.Context, caller Caller, platform, tool string, domain map[string]any) (string, error) {
	app, _ := domain["app"].(string)
	table, _ := domain["table"].(string)
	idempotencyKey, _ := domain["idempotency_key"].(string)

	records, err := parseRecords(domain["records"])
	if err != nil {
		return "", err
	}

	params := map[string]any{}
	if idempotencyKey != "" {
		params["client_token"] = idempotencyKey
	}

	var results []string
	for i, rec := range records {
		resp, err := caller.CallTool(ctx, platform, tool, map[string]any{
			"path":   map[string]any{"app_token": app, "table_id": table},
			"params": params,
			"data":   map[string]any{"fields": rec.Fields},
		})
		if err != nil {
			return "", fmt.Errorf("第 %d 条记录创建失败: %w", i+1, err)
		}
		results = append(results, resp)
	}
	return fmt.Sprintf("成功创建 %d 条记录", len(results)), nil
}

// feishuTableUpdate 飞书单条更新:record_id 在 path 里,循环调用。
func feishuTableUpdate(ctx context.Context, caller Caller, platform, tool string, domain map[string]any) (string, error) {
	app, _ := domain["app"].(string)
	table, _ := domain["table"].(string)

	records, err := parseRecords(domain["records"])
	if err != nil {
		return "", err
	}

	var results []string
	for i, rec := range records {
		if rec.ID == "" {
			return "", fmt.Errorf("第 %d 条记录缺少 id(更新必须指定记录 ID)", i+1)
		}
		resp, err := caller.CallTool(ctx, platform, tool, map[string]any{
			"path": map[string]any{"app_token": app, "table_id": table, "record_id": rec.ID},
			"data": map[string]any{"fields": rec.Fields},
		})
		if err != nil {
			return "", fmt.Errorf("第 %d 条记录更新失败: %w", i+1, err)
		}
		results = append(results, resp)
	}
	return fmt.Sprintf("成功更新 %d 条记录", len(results)), nil
}

// dingtalkTableCreate 钉钉批量创建:insertNotableRecords 一次传入所有记录。
func dingtalkTableCreate(ctx context.Context, caller Caller, platform, tool string, domain map[string]any) (string, error) {
	operatorID, _ := domain["operator_id"].(string)
	if operatorID == "" {
		return "", fmt.Errorf("table.create 在钉钉上必须提供 operator_id(操作人 unionId)")
	}
	baseID, _ := domain["app"].(string)
	sheet, _ := domain["table"].(string)

	records, err := parseRecords(domain["records"])
	if err != nil {
		return "", err
	}

	// 转换为钉钉批量结构:records: [{fields: {...}}]
	rows := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		rows = append(rows, map[string]any{"fields": rec.Fields})
	}

	return caller.CallTool(ctx, platform, tool, map[string]any{
		"operatorId":    operatorID,
		"baseId":        baseID,
		"sheetIdOrName": sheet,
		"records":       rows,
	})
}

// dingtalkTableUpdate 钉钉批量更新:record_id 在 records[].id 里。
func dingtalkTableUpdate(ctx context.Context, caller Caller, platform, tool string, domain map[string]any) (string, error) {
	operatorID, _ := domain["operator_id"].(string)
	if operatorID == "" {
		return "", fmt.Errorf("table.update 在钉钉上必须提供 operator_id(操作人 unionId)")
	}
	baseID, _ := domain["app"].(string)
	sheet, _ := domain["table"].(string)

	records, err := parseRecords(domain["records"])
	if err != nil {
		return "", err
	}

	// 钉钉要求 records[].id 非空
	rows := make([]map[string]any, 0, len(records))
	for i, rec := range records {
		if rec.ID == "" {
			return "", fmt.Errorf("第 %d 条记录缺少 id(更新必须指定记录 ID)", i+1)
		}
		rows = append(rows, map[string]any{"id": rec.ID, "fields": rec.Fields})
	}

	return caller.CallTool(ctx, platform, tool, map[string]any{
		"operatorId":    operatorID,
		"baseId":        baseID,
		"sheetIdOrName": sheet,
		"records":       rows,
	})
}

// feishuSendText 飞书发送适配器:根据 format 选择 text 或 post 消息类型。
//
// text:  content = {"text": body}
// post:  content = {"zh_cn":{"title":...,"content":[[inline...]]}}(由 MarkdownToFeishuPost 生成)
func feishuSendText(ctx context.Context, caller Caller, platform, tool string, domain map[string]any) (string, error) {
	target, _ := domain["target"].(string)
	body, _ := domain["text"].(string)
	format, _ := domain["format"].(string)
	title, _ := domain["title"].(string)

	msgType := "text"
	var content string
	if format == "markdown" {
		msgType = "post"
		var err error
		content, err = MarkdownToFeishuPost(body, title)
		if err != nil {
			return "", fmt.Errorf("markdown 转飞书富文本失败: %w", err)
		}
	} else {
		raw, err := json.Marshal(map[string]string{"text": body})
		if err != nil {
			return "", fmt.Errorf("构造飞书文本消息失败: %w", err)
		}
		content = string(raw)
	}

	return caller.CallTool(ctx, platform, tool, map[string]any{
		"params": map[string]any{"receive_id_type": "chat_id"},
		"data": map[string]any{
			"receive_id": target,
			"msg_type":   msgType,
			"content":    content,
		},
	})
}

// RegisterAdapter 注册具名适配器(供后续能力扩展或外部测试使用)。
func RegisterAdapter(name string, fn AdapterFunc) {
	if name == "" || fn == nil {
		return
	}
	adapters[name] = fn
}

// adapterNames 返回已注册适配器名(调试用)。
func adapterNames() []string {
	out := make([]string, 0, len(adapters))
	for k := range adapters {
		out = append(out, k)
	}
	return out
}

// errAdapterMissing 统一的缺失适配器错误。
func errAdapterMissing(name string) error {
	return fmt.Errorf("适配器 %q 未注册(已注册: %v)", name, adapterNames())
}
