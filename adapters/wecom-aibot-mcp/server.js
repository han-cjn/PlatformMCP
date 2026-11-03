#!/usr/bin/env node
/**
 * 企微智能机器人 MCP 适配器(本地 stdio MCP Server)
 * 基于官方 @wecom/aibot-node-sdk,把智能机器人能力包装成 MCP 工具 + 事件上报。
 *
 * 工具:
 *   - wecom_send_text: 主动发送 Markdown 消息(chatid 必填;群聊会话 ID 见 Hub 的 hub.chats)
 * 事件:
 *   - 六类消息/事件帧 -> 可靠上报到 Hub /events(内存队列 + 指数退避重试,P0-2)
 *
 * 环境变量:WECOM_BOT_ID / WECOM_BOT_SECRET(必填);HUB_EVENT_URL(可选);HUB_EVENT_SECRET(可选);
 *          HUB_EVENT_RETRY_MAX(可选,默认 5)
 */
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { z } from 'zod';
import { WSClient } from '@wecom/aibot-node-sdk';

const botId = process.env.WECOM_BOT_ID;
const secret = process.env.WECOM_BOT_SECRET;
if (!botId || !secret) {
  console.error('[wecom-aibot-mcp] 缺少 WECOM_BOT_ID / WECOM_BOT_SECRET');
  process.exit(1);
}

const HUB_EVENT_URL = process.env.HUB_EVENT_URL || '';
const HUB_EVENT_SECRET = process.env.HUB_EVENT_SECRET || '';
const RETRY_MAX = parseInt(process.env.HUB_EVENT_RETRY_MAX || '5', 10);

// ---- 可靠事件上报(P0-2):内存队列 + 指数退避;达上限丢弃并打 ERROR,防止无限堆积 ----
const queue = [];
let flushing = false;

async function flushQueue() {
  if (flushing) return;
  flushing = true;
  while (queue.length > 0) {
    const evt = queue[0];
    let delay = 1000;
    let delivered = false;
    for (let attempt = 1; attempt <= RETRY_MAX; attempt++) {
      try {
        const headers = { 'Content-Type': 'application/json' };
        if (HUB_EVENT_SECRET) headers['X-Hub-Secret'] = HUB_EVENT_SECRET;
        const res = await fetch(HUB_EVENT_URL, { method: 'POST', headers, body: JSON.stringify(evt) });
        if (res.ok) { delivered = true; break; }
        throw new Error(`HTTP ${res.status}`);
      } catch (err) {
        console.error(`[wecom-aibot-mcp] 事件投递失败(${attempt}/${RETRY_MAX}):`, err.message);
        if (attempt < RETRY_MAX) await new Promise((r) => setTimeout(r, delay));
        delay = Math.min(delay * 2, 30000);
      }
    }
    if (!delivered) {
      console.error('[wecom-aibot-mcp] ERROR 事件已达重试上限被丢弃:', JSON.stringify({ type: evt.type, source_id: evt.source_id }));
    }
    queue.shift();
  }
  flushing = false;
}

const forwardEvent = (type) => (frame) => {
  if (!HUB_EVENT_URL) return;
  const body = frame?.body || {};
  // 去重键必须用平台业务消息 ID(msgid):企微重复推送时 req_id 会变、msgid 不变
  const evt = {
    platform: 'wecom',
    type: type.startsWith('message.') ? 'im.message' : type,
    source_id: body.msgid || '',
    payload: body,
  };
  queue.push(evt);
  flushQueue();
};

// ---- 长连接(P1-4):disconnect/error 时复位 authed,等待 SDK 自动重连后再放行 ----
let authed = false;
const wsClient = new WSClient({ botId, secret });
try {
  wsClient.on('disconnected', () => {
    authed = false;
    console.error('[wecom-aibot-mcp] 长连接断开,等待 SDK 自动重连');
  });
  wsClient.on('error', (err) => {
    console.error('[wecom-aibot-mcp] 连接错误:', err?.message || err);
  });
} catch { /* 旧版 SDK 无这些事件时忽略,重连仍由 SDK 内置 */ }

wsClient.on('authenticated', () => {
  authed = true;
  console.error('[wecom-aibot-mcp] 长连接认证成功');
});
for (const t of ['message.text', 'message.image', 'message.mixed', 'event.enter_chat', 'event.template_card_event', 'message.feedback']) {
  wsClient.on(t, forwardEvent(t));
}
wsClient.connect();

const waitAuth = async () => {
  const deadline = Date.now() + 15000;
  while (!authed && Date.now() < deadline) await new Promise((r) => setTimeout(r, 200));
  if (!authed) throw new Error('企微长连接未就绪(未认证或已断开)');
};

// ---- MCP Server ----
const server = new McpServer({ name: 'wecom-aibot-mcp', version: '0.3.0' });

server.tool(
  'wecom_send_text',
  '企业微信智能机器人:向指定会话主动发送 Markdown 消息(chatid 可从 Hub hub.chats 枚举,或从消息帧 payload.chatid 获取)',
  { chatid: z.string().describe('目标会话 ID(群聊/单聊)'), content: z.string().describe('Markdown 消息内容') },
  async ({ chatid, content }) => {
    try {
      await waitAuth();
      await wsClient.sendMessage(chatid, { msgtype: 'markdown', markdown: { content } });
      return { content: [{ type: 'text', text: `已发送到 ${chatid}` }] };
    } catch (err) {
      if (String(err?.message || '').includes('未就绪')) authed = false;
      return { content: [{ type: 'text', text: `发送失败: ${err.message}` }], isError: true };
    }
  },
);

await server.connect(new StdioServerTransport());
console.error('[wecom-aibot-mcp] MCP server 已启动 (stdio)');
