#!/usr/bin/env python3
"""钉钉 Stream 探针:捕获机器人被 @的群消息,打印 openConversationId(conversationId)。
仅供联调定位群会话 ID 使用;M2 事件桥将复用此通道。"""
import os
import dingtalk_stream
from dingtalk_stream import ChatbotMessage


class ProbeHandler(dingtalk_stream.ChatbotHandler):
    async def process(self, callback: dingtalk_stream.CallbackMessage):
        msg = ChatbotMessage.from_dict(callback.data)
        cid = msg.conversation_id
        ctype = msg.conversation_type  # '2'=群聊
        sender = msg.sender_staff_id or msg.sender_id
        text = msg.text.content.strip() if msg.text else ""
        print(f"[probe] conversation_type={ctype} openConversationId={cid} sender={sender} text={text}", flush=True)
        self.reply_text("收到,Hub 联调探针在线", msg)
        return 0, "ok"


def main():
    credential = dingtalk_stream.Credential(
        os.environ["DINGTALK_Client_ID"], os.environ["DINGTALK_Client_Secret"]
    )
    client = dingtalk_stream.DingTalkStreamClient(credential)
    client.register_callback_handler(
        dingtalk_stream.ChatbotMessage.TOPIC, ProbeHandler()
    )
    print("[probe] Stream 已连接,等待群消息...", flush=True)
    client.start_forever()


if __name__ == "__main__":
    main()
