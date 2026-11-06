package semantic

// 统一能力清单。
//
// 平台差异用【声明式映射】表达,不为每个平台写 Go 分支。新增平台只需在此
// 追加一段 PlatformMapping;能力本身(字段名/必填/默认值)对所有平台一致。

// commonTargetDesc 会话标识的四平台叫法差异说明,统一收敛为 target。
const commonTargetDesc = "目标会话 ID。四平台叫法不同(飞书 chat_id / 钉钉 openConversationId / 企微 chatid),此处统一传 target"

// ImSendText 发送消息(纯文本或 Markdown)。
//
// 关于 format 的设计说明(2026-11 实测):
//   - 钉钉 sendMessageToGroupByRobot 的 msgParam.text 官方描述即为「markdown格式」
//   - 企微 wecom_send_text 实际发送 msgtype=markdown、markdown.content
//
// 也就是说这两个平台"文本"接口本身就按 markdown 渲染,无需区分。
// 只有飞书需要区分:text 类型按纯文本显示,markdown 必须走 msg_type=post 富文本,
// 且 content 要求嵌套结构 —— 声明式映射表达不了,用 Adapter 兜底转换。
// 因此不设独立的 im.send_markdown 能力(在钉钉/企微上会与 send_text 完全重复),
// 而是用 format 参数表达"要不要按 markdown 渲染"。
var ImSendText = &Capability{
	Name:  "im.send_text",
	Title: "发送消息",
	Description: "向指定会话发送消息。format=plain 为纯文本;format=markdown 按 Markdown 渲染" +
		"(飞书自动转换为富文本 post,钉钉/企微原生支持)。客户端只需 target + text",
	Schema: map[string]FieldSpec{
		"target": {
			Type: "string",
			Desc: commonTargetDesc,
		},
		"text": {
			Type: "string",
			Desc: "消息正文。format=markdown 时支持 **粗体**、*斜体*、`代码`、~~删除线~~、[链接](url)、# 标题、- 列表",
		},
		"title": {
			Type:    "string",
			Desc:    "可选标题。钉钉/飞书(post)生效;未提供时飞书取正文首个 # 标题",
			Default: "",
		},
		"format": {
			Type:    "string",
			Desc:    "渲染格式:plain(纯文本,默认)或 markdown",
			Enum:    []string{"plain", "markdown"},
			Default: "plain",
		},
	},
	Required: []string{"target", "text"},
	Platforms: map[string]PlatformMapping{
		// 飞书:Adapter 兜底——format=plain 走 text,format=markdown 转 post 富文本
		"feishu": {
			Tool:    "im_v1_message_create",
			Adapter: "feishu.send_text",
		},
		// 钉钉:msgParam.text 原生按 markdown 渲染,plain/markdown 用同一路径
		"dingtalk": {
			Tool: "sendMessageToGroupByRobot",
			Args: map[string]string{
				"openConversationId": "$.target",
			},
			Encode: []FieldEncode{{
				Target: "msgParam",
				Format: "json",
				From: map[string]string{
					"title": "$.title ?? MCP Hub",
					"text":  "$.text",
				},
			}},
		},
		// 企微:wecom_send_text 内部即 msgtype=markdown,content 直接传原文
		"wecom": {
			Tool: "wecom_send_text",
			Args: map[string]string{
				"chatid":  "$.target",
				"content": "$.text",
			},
		},
		// 腾讯文档没有 IM 能力:显式声明,给出可读原因而非"不支持平台"的模糊报错
		"tencent-docs": {
			Unsupported: "腾讯文档无 IM 消息能力",
		},
	},
}

// ---- 文档能力 ----
//
// 平台能力是不对称的,如实声明而非强行统一:
//   - 腾讯文档:doc.create_with_markdown 建文档(需 base64)、doc.insert_markdown 按位置追加
//   - 飞书:docx_builtin_import 导入整篇 markdown(等价于"创建"),但没有"向已有文档追加"的工具
//     (docx_v1_document_rawContent 只能读),因此 doc.append 在飞书显式 Unsupported
//   - 钉钉/企微:无文档工具

// DocCreate 用 Markdown 创建一篇文档,返回 file_id / url。
var DocCreate = &Capability{
	Name:        "doc.create",
	Title:       "创建文档",
	Description: "用 Markdown 创建一篇新文档,返回文档 ID 与访问链接。两平台行为差异:腾讯返回 file_id/url;飞书为导入任务,返回文档 token",
	Schema: map[string]FieldSpec{
		"title": {Type: "string", Desc: "文档标题"},
		"markdown": {
			Type: "string",
			Desc: "Markdown 内容(标题/列表/粗体/代码块/链接等)",
		},
	},
	Required: []string{"title", "markdown"},
	Platforms: map[string]PlatformMapping{
		// 腾讯:base64_markdown 必须编码,用 Adapter 处理(base64 不是字段映射能表达的)
		"tencent-docs": {
			Tool:    "doc.create_with_markdown",
			Adapter: "tencent.doc_create",
		},
		// 飞书:导入整篇 markdown,data.file_name + data.markdown
		"feishu": {
			Tool: "docx_builtin_import",
			Args: map[string]string{
				"data.file_name": "$.title",
				"data.markdown":  "$.markdown",
			},
		},
		"dingtalk": {Unsupported: "钉钉无在线文档创建能力"},
		"wecom":    {Unsupported: "企业微信无在线文档创建能力"},
	},
}

// DocAppend 向已有文档追加 Markdown 内容。
var DocAppend = &Capability{
	Name:        "doc.append",
	Title:       "向文档追加内容",
	Description: "向已有文档追加 Markdown 内容。腾讯在指定位置插入(默认开头);飞书不支持追加,仅支持整篇导入",
	Schema: map[string]FieldSpec{
		"doc_id":   {Type: "string", Desc: "文档 ID(腾讯 file_id)"},
		"markdown": {Type: "string", Desc: "要追加的 Markdown 内容"},
		"position": {
			Type:    "number",
			Desc:    "插入位置索引(仅腾讯生效,默认 0=开头)",
			Default: 0,
		},
	},
	Required: []string{"doc_id", "markdown"},
	Platforms: map[string]PlatformMapping{
		"tencent-docs": {
			Tool: "doc.insert_markdown",
			Args: map[string]string{
				"file_id":  "$.doc_id",
				"idx":      "$.position",
				"markdown": "$.markdown",
			},
		},
		"feishu":   {Unsupported: "飞书 docx 暂无追加/插入工具,只能整篇导入(doc.create)"},
		"dingtalk": {Unsupported: "钉钉无在线文档编辑能力"},
		"wecom":    {Unsupported: "企业微信无在线文档能力"},
	},
}

// DocSearch 按关键字搜索文档。
var DocSearch = &Capability{
	Name:        "doc.search",
	Title:       "搜索文档",
	Description: "按关键字搜索在线文档,返回标题/类型/链接等。两平台入参归一为 keyword",
	Schema: map[string]FieldSpec{
		"keyword": {Type: "string", Desc: "搜索关键字(按文档标题匹配)"},
	},
	Required: []string{"keyword"},
	Platforms: map[string]PlatformMapping{
		"tencent-docs": {
			Tool: "manage.search_file",
			Args: map[string]string{"search_key": "$.keyword"},
		},
		"feishu": {
			Tool: "docx_builtin_search",
			Args: map[string]string{"data.search_key": "$.keyword"},
		},
		"dingtalk": {Unsupported: "钉钉无文档搜索工具"},
		"wecom":    {Unsupported: "企业微信无文档搜索能力"},
	},
}

// ---- 表格能力 ----
//
// table.query 是最典型的「最小公约数」能力:两平台词汇表、条件结构、分页键完全不同,
// 只能收敛到领域字段(app/table/filters/conjunction/page_size),由 Adapter 翻译。
// 领域操作符用统一词汇(eq/contains/gt...),平台映射见 tablequery.go 的 opMap。

// TableQuery 查询表格记录。
var TableQuery = &Capability{
	Name:  "table.query",
	Title: "查询表格记录",
	Description: "按条件查询多维表格/智能表格记录。filters 为 [{field,op,value}] 数组," +
		"op 用统一词汇(eq/ne/contains/not_contains/empty/not_empty/gt/gte/lt/lte),平台自动翻译",
	Schema: map[string]FieldSpec{
		"app": {
			Type: "string",
			Desc: "表格应用 ID(飞书 app_token / 钉钉 baseId)",
		},
		"table": {
			Type: "string",
			Desc: "数据表 ID(飞书 table_id / 钉钉 sheetIdOrName)",
		},
		"filters": {
			Type: "array",
			Desc: "筛选条件:[{field,op,value}]。op 缺省 eq",
		},
		"conjunction": {
			Type:    "string",
			Desc:    "条件组合:and(默认)/or",
			Enum:    []string{"and", "or"},
			Default: "and",
		},
		"page_size": {
			Type:    "number",
			Desc:    "每页记录数(飞书最大 500,钉钉最大 100;默认 100)",
			Default: 100,
		},
		"operator_id": {
			Type: "string",
			Desc: "操作人 unionId(仅钉钉必填,平台特有透传字段)",
		},
	},
	Required: []string{"app", "table"},
	Platforms: map[string]PlatformMapping{
		"feishu": {
			Tool:    "bitable_v1_appTableRecord_search",
			Adapter: "feishu.table_query",
		},
		"dingtalk": {
			Tool:    "listNotableRecords",
			Adapter: "dingtalk.table_query",
		},
		"wecom":        {Unsupported: "企业微信无在线表格能力"},
		"tencent-docs": {Unsupported: "腾讯文档在线表格查询请用 sheet.* 系列工具(结构差异大,暂未收敛)"},
	},
}

// TableCreate 批量插入表格记录。
//
// 平台差异:
//   - 飞书:单条 API(bitable_v1_appTableRecord_create),Adapter 循环调用
//   - 钉钉:批量 API(insertNotableRecords),records 数组一次传入
//
// 领域模型统一为批量(records 数组),飞书 Adapter 内部循环调用。
var TableCreate = &Capability{
	Name:  "table.create",
	Title: "插入表格记录",
	Description: "向表格插入一条或多条记录。records 为 [{fields: {字段名: 值}}] 数组;" +
		"飞书按单条循环插入,钉钉批量一次插入。幂等:飞书可用 idempotency_key",
	Schema: map[string]FieldSpec{
		"app": {
			Type: "string",
			Desc: "表格应用 ID(飞书 app_token / 钉钉 baseId)",
		},
		"table": {
			Type: "string",
			Desc: "数据表 ID(飞书 table_id / 钉钉 sheetIdOrName)",
		},
		"records": {
			Type: "array",
			Desc: "记录数组:[{fields: {字段名: 值}}]。字段值格式参考各平台字段类型",
		},
		"idempotency_key": {
			Type: "string",
			Desc: "幂等键(仅飞书生效,格式 uuidv4;同键重复调用不产生重复记录)",
		},
		"operator_id": {
			Type: "string",
			Desc: "操作人 unionId(仅钉钉必填)",
		},
	},
	Required: []string{"app", "table", "records"},
	Platforms: map[string]PlatformMapping{
		"feishu": {
			Tool:    "bitable_v1_appTableRecord_create",
			Adapter: "feishu.table_create",
		},
		"dingtalk": {
			Tool:    "insertNotableRecords",
			Adapter: "dingtalk.table_create",
		},
		"wecom":        {Unsupported: "企业微信无在线表格能力"},
		"tencent-docs": {Unsupported: "腾讯文档在线表格写入请用 sheet.* 系列工具(结构差异大,暂未收敛)"},
	},
}

// TableUpdate 批量更新表格记录。
//
// 平台差异:
//   - 飞书:单条 API(bitable_v1_appTableRecord_update),record_id 在 path 里
//   - 钉钉:批量 API(updateNotableRecords),record_id 在 records[].id 里
//
// 领域模型统一为批量,record_id 放在每条记录的 id 字段。
var TableUpdate = &Capability{
	Name:  "table.update",
	Title: "更新表格记录",
	Description: "更新表格中已有记录。records 为 [{id: 记录ID, fields: {字段名: 新值}}] 数组;" +
		"飞书按单条循环更新,钉钉批量一次更新",
	Schema: map[string]FieldSpec{
		"app": {
			Type: "string",
			Desc: "表格应用 ID(飞书 app_token / 钉钉 baseId)",
		},
		"table": {
			Type: "string",
			Desc: "数据表 ID(飞书 table_id / 钉钉 sheetIdOrName)",
		},
		"records": {
			Type: "array",
			Desc: "记录数组:[{id: 记录ID, fields: {字段名: 新值}}]",
		},
		"operator_id": {
			Type: "string",
			Desc: "操作人 unionId(仅钉钉必填)",
		},
	},
	Required: []string{"app", "table", "records"},
	Platforms: map[string]PlatformMapping{
		"feishu": {
			Tool:    "bitable_v1_appTableRecord_update",
			Adapter: "feishu.table_update",
		},
		"dingtalk": {
			Tool:    "updateNotableRecords",
			Adapter: "dingtalk.table_update",
		},
		"wecom":        {Unsupported: "企业微信无在线表格能力"},
		"tencent-docs": {Unsupported: "腾讯文档在线表格写入请用 sheet.* 系列工具(结构差异大,暂未收敛)"},
	},
}

// 内置能力清单(新增能力在此注册,工具与矩阵自动生效)。
var (
	builtinCapabilities = []*Capability{
		ImSendText,
		DocCreate,
		DocAppend,
		DocSearch,
		TableQuery,
		TableCreate,
		TableUpdate,
	}
)

// BuiltinCatalog 返回内置能力目录。
func BuiltinCatalog() *Catalog {
	return NewCatalog(builtinCapabilities...)
}
