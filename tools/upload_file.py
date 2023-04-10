import os
import sys
from asyncio import CancelledError

from telethon import TelegramClient

from tools.tqdm import TqdmUpTo


async def upload_file(client: TelegramClient, chat_id, folder_path: str):
    peo = await client.get_entity(chat_id)
    # 遍历文件夹下的所有文件
    for filename in os.listdir(folder_path):
        file_path = os.path.join(folder_path, filename)
        # 判断文件是否为视频文件
        if os.path.isfile(file_path):
            try:
                with TqdmUpTo(unit='B', unit_scale=True, unit_divisor=1024, total=os.path.getsize(file_path),
                              bar_format=TqdmUpTo.bar_format, desc=os.path.basename(file_path)) as bar:
                    # 上传文件到Telegram服务器
                    result = await client.upload_file(file_path, progress_callback=bar.update_to)
                    # 发送文件到指定的群组或频道
                    await client.send_file(peo, result, caption=os.path.basename(file_path))
            except CancelledError:
                print("取消上传")
                sys.exit()
            except Exception as e:
                print("上传出错", e.__class__.__name__)
