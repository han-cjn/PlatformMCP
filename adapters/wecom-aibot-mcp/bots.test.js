import assert from 'node:assert/strict';
import test from 'node:test';
import { parseBotConfigs, selectBot, eventForBot, formatSendError } from './bots.js';

const bots = [
  { name: 'mcp-hub', bot_id: 'id-1', secret: 'secret-1' },
  { name: '测试', bot_id: 'id-2', secret: 'secret-2' },
];

test('多机器人未选择时返回候选,绝不回退第一项', () => {
  assert.throws(() => selectBot(bots), (err) => {
    assert.equal(err.code, 'NEED_BOT_SELECTION');
    const result = JSON.parse(err.message);
    assert.equal(result.phase, 'need_bot_selection');
    assert.deepEqual(result.bots, [{ name: 'mcp-hub' }, { name: '测试' }]);
    assert.equal(result.next.includes('未发送消息'), true);
    assert.equal(err.message.includes('secret-1'), false);
    return true;
  });
});

test('未知机器人名不得自动改选其他机器人', () => {
  assert.throws(() => selectBot(bots, '不存在的机器人'), (err) => {
    assert.equal(err.code, 'UNKNOWN_BOT');
    assert.equal(JSON.parse(err.message).phase, 'unknown_bot');
    return true;
  });
});

test('明确选择后精确路由,单机器人兼容缺省', () => {
  assert.equal(selectBot(bots, '测试').bot_id, 'id-2');
  assert.equal(selectBot(bots.slice(0, 1)).name, 'mcp-hub');
  assert.deepEqual(parseBotConfigs({ WECOM_BOTS: JSON.stringify(bots) }), bots);
});

test('发送失败保留企微错误帧的真实诊断', () => {
  const direct = JSON.parse(formatSendError({ errcode: 60020, errmsg: 'not allow to access from your ip', headers: { req_id: 'req-1' } }));
  assert.deepEqual(direct, { errcode: 60020, errmsg: 'not allow to access from your ip', req_id: 'req-1' });
  assert.equal(formatSendError({ errcode: 60020, errmsg: 'denied' }).includes('[object Object]'), false);

  const nested = JSON.parse(formatSendError({ body: { errcode: 301002, errmsg: 'invalid chatid' } }));
  assert.deepEqual(nested, { errcode: 301002, errmsg: 'invalid chatid', req_id: null });
});

test('发送失败兼容字符串、Error 与无诊断异常', () => {
  assert.equal(formatSendError('network unavailable'), 'network unavailable');
  assert.deepEqual(JSON.parse(formatSendError(new Error('socket closed'))), {
    errcode: null, errmsg: 'socket closed', req_id: null,
  });
  assert.equal(formatSendError({}), '未知错误(SDK 未返回可序列化的诊断)');
  assert.equal(formatSendError(undefined), '未知错误(SDK 未返回可序列化的诊断)');
});

test('事件保留来源机器人并隔离相同 msgid', () => {
  const frame = { body: { msgid: 'same-msg', chatid: 'chat-1', text: { content: 'hello' } } };
  const a = eventForBot(bots[0], 'message.text', frame);
  const b = eventForBot(bots[1], 'message.text', frame);
  assert.equal(a.source_id, 'mcp-hub:same-msg');
  assert.equal(b.source_id, '测试:same-msg');
  assert.equal(a.payload.bot_name, 'mcp-hub');
  assert.equal(b.payload.bot_name, '测试');
  assert.equal(a.payload.text.content, 'hello');
});
