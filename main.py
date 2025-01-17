import json
import os
import time
import random
import re

from telethon import TelegramClient, events, types
from telethon.errors import SessionPasswordNeededError

CONFIG_PATH = "config.json"

def load_config(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)

def ensure_dir(dir_path):
    if not os.path.exists(dir_path):
        os.makedirs(dir_path, exist_ok=True)

def main():
    # 1. 读取 config.json
    cfg = load_config(CONFIG_PATH)
    api_id = cfg["api_id"]
    api_hash = cfg["api_hash"]
    phone_number = cfg["phone_number"]
    session_name = cfg["session_name"]
    video_dir = cfg["video_dir"]

    # 2. 创建 Telethon 客户端
    #    session_name 可以是 "./session/mysession"，保证文件在 session 目录下
    session_path = os.path.join("session", session_name)
    client = TelegramClient(session=session_path, api_id=api_id, api_hash=api_hash)

    # 3. 同步方式启动 Telethon
    with client:
        # 若尚未登录则执行登录流程
        if not client.is_user_authorized():
            print("未检测到有效会话，需要登录...")
            client.start(phone=lambda: phone_number)  # 自动输入手机号

            # Telethon 会发送验证码到手机号
            # 如果是第一次运行，会自动提示你在命令行输入收到的验证码
            # 如果有 2FA，会在这里抛出 SessionPasswordNeededError
            if client.is_user_authorized():
                print("验证码验证成功!")
            else:
                try:
                    if client.is_user_authorized():
                        print("已登录。")
                    else:
                        # 如果账号设置了2FA密码
                        raise SessionPasswordNeededError
                except SessionPasswordNeededError:
                    pwd = input("请输入 2FA 密码(若无则直接回车): ")
                    client.sign_in(password=pwd)
                    print("2FA 验证成功。")

        # 确保 videos 目录存在
        ensure_dir(video_dir)

        print("登录成功或已是登录状态，开始监听消息...")

        # 4. 添加一个事件处理器，监听“自己”发送的消息
        #    Telethon 提供 @events.register() 或 client.add_event_handler()
        #    我们通过 "from_me=True" 来仅处理自己发送的消息
        @client.on(events.NewMessage(from_users="me"))
        async def handler(event):
            text = event.message.message.strip().lower()
            if text.startswith("download "):
                # 提取频道/群链接
                # 例如 "download https://t.me/xxxx"
                # 简单用正则或 split
                parts = event.message.message.split(" ", 1)
                if len(parts) < 2:
                    return
                link = parts[1].strip()
                channel_username = parse_channel_username(link)

                # 异步执行下载
                await download_channel_videos(client, event, channel_username, video_dir)

        # 5. 进入阻塞状态，直到 ctrl+c 退出
        print("等待新消息中...按 Ctrl+C 退出。")
        client.run_until_disconnected()

async def download_channel_videos(client: TelegramClient, event: events.NewMessage.Event, channel_username: str, video_dir: str):
    # 通知
    msg = event.message
    await msg.reply(f"开始下载频道 {channel_username} 的视频...")

    # 尝试获取对应频道实体
    try:
        entity = await client.get_entity(channel_username)
    except ValueError:
        # 若无法解析，可能是私有群/频道，或不存在
        await msg.reply(f"解析频道/群 {channel_username} 失败，可能是私有链接。")
        return

    # 拉取所有消息时，需要从最早到最新/或最新到最早
    # Telethon 提供了 "client.iter_messages" 可迭代所有消息
    # 默认是从最新到最早，我们可以加 reverse=True 从最早到最新
    count = 0

    async for message in client.iter_messages(entity, reverse=True):
        if not message.media:
            continue

        # 判断是否是视频
        # 在 Telethon 中常见的 video 类型：MessageMediaDocument，DocumentAttributeVideo
        if message.video or (message.document and any(isinstance(attr, types.DocumentAttributeVideo) for attr in message.document.attributes)):
            # 下载文件到指定目录
            count += 1
            print(f"[{count}] 正在下载 msg_id={message.id} 的视频...")
            try:
                # Telethon 提供 .download_media()
                await message.download_media(file=video_dir)
                print(f"下载完成: msg_id={message.id}")
            except Exception as e:
                print(f"下载出错(msg_id={message.id}): {e}")

            # 完成一个视频后，等待随机 5~10分钟
            delay = random.randint(5, 10) * 60
            print(f"等待 {delay//60} 分钟后再下一个视频...")
            time.sleep(delay)

    # 全部下载完成后，把原来的“download ...”消息改为 finished
    # 注意：若消息发送超过一定时限(48小时)，可能无法编辑
    try:
        await client.edit_message(event.message.peer_id, event.message.id, text="finished")
    except Exception as e:
        print(f"编辑消息为 finished 失败: {e}")

def parse_channel_username(link: str) -> str:
    """
    将 'https://t.me/xxxx' / 't.me/xxxx' 等转换成 'xxxx'
    """
    link = link.strip()
    link = re.sub(r"^https?://t\.me/", "", link)
    link = re.sub(r"^t\.me/", "", link)
    return link

if __name__ == "__main__":
    main()
