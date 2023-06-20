import os
import re
import sys
from asyncio import CancelledError
from datetime import datetime

import demoji
from telethon import TelegramClient
from telethon.errors import FileReferenceExpiredError

from tools.tool import GetFileName, getHistoryMessage, GetChatId
from tools.tqdm import TqdmUpTo


# TODO 或许能加快下载速度的方法（https://gist.github.com/painor/7e74de80ae0c819d3e9abcf9989a8dd6）
# fileExist 检查文件是否存在（文件名和大小都相等），如果不存在重名文件加序号
def fileExist(file_path: str, file_size):
    i = 2
    ix = file_path.rfind('.', 1)
    fileName = file_path[:ix]
    fileType = file_path[ix:]
    temp = file_path
    while os.path.exists(temp):
        if os.path.getsize(temp) == file_size:
            return True, temp
        temp = f'{fileName}({i}){fileType}'
        i += 1
    return False, temp


def GetFileSuffix(message) -> list:
    mime_type = 'unknown/unknown'
    if hasattr(message.media, 'document'):
        mime_type = message.media.document.mime_type
    elif hasattr(message.media, 'photo'):
        mime_type = 'image/jpg'
    return mime_type.split('/')


async def download_file(client: TelegramClient, channel_title, channel_id, message, old=False):
    media_type = GetFileSuffix(message)[0]
    # 获取媒体类型
    is_photo = media_type == 'image'
    is_video = media_type == 'video'
    is_audio = media_type == 'audio'

    message_time = message.date
    formatted_time = datetime.strftime(message_time, '%Y_%m')

    # 如果不是文件就放弃（可能是音频文字啥的）
    if not (is_photo or is_video or is_audio):
        return
    file_name = GetFileName(message)
    file_path = f'{os.environ["save_path"]}/{channel_title}-{channel_id}/{file_name}'
    new_file_path = f'{os.environ["save_path"]}/{channel_title}-{channel_id}/{formatted_time}/{file_name}'
    file_size = message.file.size
    ret, file_path = fileExist(file_path, file_size)
    if not ret:
        # 已经判断文件不存在，并且保证了文件名不重复
        download_path = file_path + '.downloading'
        print(f"开始下载：{file_name}")
        try:
            with TqdmUpTo(total=file_size, bar_format=TqdmUpTo.bar_format, desc=file_name[:10]) as bar:
                await message.download_media(download_path, progress_callback=bar.update_to)
        except CancelledError:
            print("取消下载")
            os.remove(download_path)
            sys.exit()
        except FileReferenceExpiredError:
            if old:
                print('重试失败，退出下载')
                exit(1)
            print('下载超时，重试中')
            channelData = await client.get_entity(int(channel_id))
            newMessages = client.iter_messages(entity=channelData, ids=message.id)
            async for newMessage in newMessages:
                await download_file(client, channel_title, channel_id, newMessage, old=True)
        except Exception as e:
            print("下载出错", e.__class__.__name__)
            os.remove(download_path)
        else:
            os.rename(download_path, file_path)
    else:
        print(f"媒体已存在：{file_path}")


async def down_group(client: TelegramClient, chat_id, plus_func: str):
    chat_id = await GetChatId(client, chat_id)
    channel_title, messages = await getHistoryMessage(client, chat_id, plus_func)  # messages是倒序的
    async for message in messages:
        """转发消息
        await message.forward_to('me')
        """
        if message.media is not None:
            await download_file(client, channel_title, chat_id, message)
    print(channel_title, '全部下载完成')
