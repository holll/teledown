import os
import sys
from asyncio import CancelledError

from telethon import TelegramClient

from tools.tool import get_all_files
from tools.tqdm import TqdmUpTo


async def upload_file(client: TelegramClient, chat_id, path: str):
    peo = await client.get_entity(chat_id)
    path_list = []
    if os.path.isfile(path):
        path_list.append(path)
    else:
        path_list = get_all_files(path)
    # 遍历文件夹下的所有文件
    for file_path in path_list:
        # 文件预处理，解析信息
        if os.path.splitext(file_path)[1].lower() == '.mp4':
            supports_streaming = True
        else:
            supports_streaming = False
        file_caption = os.path.basename(file_path)
        file_size = os.path.getsize(file_path)
        # 发送文件到指定的群组或频道
        try:
            with TqdmUpTo(total=file_size, desc=file_caption) as bar:
                # 上传文件到Telegram服务器
                result = await client.upload_file(file_path, progress_callback=bar.update_to)
                await client.send_file(peo, result, caption=file_caption, supports_streaming=supports_streaming)
        except CancelledError:
            print("取消上传")
            sys.exit()
        except Exception as e:
            print("上传出错", e.__class__.__name__)
