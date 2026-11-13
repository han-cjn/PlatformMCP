export function parseBotConfigs(env = process.env) {
  let bots;
  if (env.WECOM_BOTS?.trim()) {
    try {
      bots = JSON.parse(env.WECOM_BOTS);
    } catch {
      throw new Error('WECOM_BOTS 不是合法 JSON');
    }
    if (!Array.isArray(bots) || bots.length === 0) {
      throw new Error('WECOM_BOTS 必须是非空机器人数组');
    }
  } else {
    bots = [{ name: 'default', bot_id: env.WECOM_BOT_ID, secret: env.WECOM_BOT_SECRET }];
  }
  const seen = new Set();
  for (const [index, bot] of bots.entries()) {
    if (!bot || typeof bot.name !== 'string' || !bot.name.trim() ||
        typeof bot.bot_id !== 'string' || !bot.bot_id.trim() ||
        typeof bot.secret !== 'string' || !bot.secret.trim()) {
      throw new Error(`机器人配置[${index}]的 name / bot_id / secret 均不能为空`);
    }
    if (seen.has(bot.name)) throw new Error(`机器人名称重复: ${bot.name}`);
    seen.add(bot.name);
  }
  return bots;
}

// 多机器人缺少明确选择时绝不回退到第一项;错误只含名称,不泄漏凭据。
export function selectBot(bots, botName = '') {
  const wanted = botName.trim();
  if (!wanted) {
    if (bots.length === 1) return bots[0];
    const err = new Error(JSON.stringify({
      phase: 'need_bot_selection',
      bots: bots.map(({ name }) => ({ name })),
      next: '请询问用户使用哪个企业微信机器人,并带 bot_name 重试;本次未发送消息',
    }));
    err.code = 'NEED_BOT_SELECTION';
    throw err;
  }
  const bot = bots.find((item) => item.name === wanted);
  if (!bot) {
    const err = new Error(JSON.stringify({
      phase: 'unknown_bot',
      bot_name: wanted,
      bots: bots.map(({ name }) => ({ name })),
      next: '请从候选机器人中明确选择;本次未发送消息',
    }));
    err.code = 'UNKNOWN_BOT';
    throw err;
  }
  return bot;
}

// SDK 主动发送失败时会以完整错误帧 reject({headers,errcode,errmsg})。
// 不得用 String(object) 把平台诊断吞成 [object Object];只抽取安全的协议字段。
export function formatSendError(err) {
  if (err && typeof err === 'object') {
    const code = err.errcode ?? err.body?.errcode;
    const message = err.errmsg ?? err.body?.errmsg ?? err.message;
    if (code !== undefined || message) {
      return JSON.stringify({
        errcode: code ?? null,
        errmsg: message || '企微返回错误但未提供 errmsg',
        req_id: err.headers?.req_id || null,
      });
    }
  }
  if (typeof err === 'string' && err.trim()) return err;
  return '未知错误(SDK 未返回可序列化的诊断)';
}

export function eventForBot(bot, type, frame) {
  const body = frame?.body || {};
  return {
    platform: 'wecom',
    type: type.startsWith('message.') ? 'im.message' : type,
    source_id: `${bot.name}:${body.msgid || ''}`,
    payload: { ...body, bot_name: bot.name },
  };
}
