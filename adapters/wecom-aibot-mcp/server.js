#!/usr/bin/env node
/**
 * 企微智能机器人 MCP 适配器:一个进程管理一个或多个命名机器人。
 * 多机器人发送必须显式传 bot_name;绝不默认选择第一项。
 *
 * 兼容旧版 WECOM_BOT_ID / WECOM_BOT_SECRET;多机器人由 Hub 从 config.json
 * 展开为 WECOM_BOTS=[{name,bot_id,secret}],只注入本进程,不写日志。
 */
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { z } from 'zod';
import { WSClient } from '@wecom/aibot-node-sdk';
import { parseBotConfigs, selectBot, eventForBot, formatSendError } from './bots.js';

const bots = parseBotConfigs();
const HUB_EVENT_URL = process.env.HUB_EVENT_URL ?? 'http://127.0.0.1:9780/events';
const HUB_EVENT_SECRET = process.env.HUB_EVENT_SECRET || '';
const RETRY_MAX = parseInt(process.env.HUB_EVENT_RETRY_MAX || '5', 10);
console.error(`[wecom-aibot-mcp] 机器人数量: ${bots.length};事件上报: ${HUB_EVENT_URL || '(已关闭)'}`);

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
    if (!delivered) console.error('[wecom-aibot-mcp] ERROR 事件已达重试上限被丢弃:', evt.type);
    queue.shift();
  }
  flushing = false;
}

const clients = new Map();
for (const bot of bots) {
  const runtime = { authed: false, client: new WSClient({ botId: bot.bot_id, secret: bot.secret }) };
  clients.set(bot.name, runtime);
  const ws = runtime.client;
  try {
    ws.on('disconnected', () => {
      runtime.authed = false;
      console.error(`[wecom-aibot-mcp] ${bot.name}:长连接断开,等待 SDK 自动重连`);
    });
    ws.on('error', (err) => console.error(`[wecom-aibot-mcp] ${bot.name}:连接错误:`, err?.message || err));
  } catch { /* 旧版 SDK 无这些事件时忽略 */ }
  ws.on('authenticated', () => {
    runtime.authed = true;
    console.error(`[wecom-aibot-mcp] ${bot.name}:长连接认证成功`);
  });
  for (const type of ['message.text', 'message.image', 'message.mixed', 'event.enter_chat', 'event.template_card_event', 'message.feedback']) {
    ws.on(type, (frame) => {
      if (!HUB_EVENT_URL) return;
      queue.push(eventForBot(bot, type, frame));
      flushQueue();
    });
  }
  ws.connect();
}

const waitAuth = async (runtime) => {
  const deadline = Date.now() + 15000;
  while (!runtime.authed && Date.now() < deadline) await new Promise((r) => setTimeout(r, 200));
  if (!runtime.authed) throw new Error('企微长连接未就绪(未认证或已断开)');
};

const server = new McpServer({ name: 'wecom-aibot-mcp', version: '0.4.0' });
server.tool(
  'wecom_send_text',
  '向指定企微会话发送 Markdown;多个机器人时必须明确 bot_name,不得自动选择',
  {
    chatid: z.string().describe('目标会话 ID'),
    content: z.string().describe('Markdown 消息内容'),
    bot_name: z.string().optional().describe('机器人名称;多个机器人时由用户明确选择'),
  },
  async ({ chatid, content, bot_name }) => {
    let selected;
    try {
      // 先消歧,再做认证等待:歧义调用不得触发任何发送。
      selected = selectBot(bots, bot_name || '');
      const runtime = clients.get(selected.name);
      await waitAuth(runtime);
      await runtime.client.sendMessage(chatid, { msgtype: 'markdown', markdown: { content } });
      return { content: [{ type: 'text', text: `已由机器人「${selected.name}」发送到 ${chatid}` }] };
    } catch (err) {
      if (selected && String(err?.message || '').includes('未就绪')) clients.get(selected.name).authed = false;
      // SDK 主动发送失败会 reject 普通错误帧,必须保留 errcode/errmsg/req_id。
      const detail = formatSendError(err);
      return { content: [{ type: 'text', text: `发送失败: ${detail}` }], isError: true };
    }
  },
);

await server.connect(new StdioServerTransport());
console.error('[wecom-aibot-mcp] MCP server 已启动 (stdio)');
