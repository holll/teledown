import os
from datetime import datetime

from telethon import TelegramClient
from telethon.errors import FileReferenceExpiredError
from telethon.tl.types import MessageMediaDocument, MessageMediaPhoto, DocumentAttributeSticker

from tools.tool import get_file_name, get_history_message, get_chat_id, match_wildcard, parse_user_ids
from tools.tqdm import TqdmUpTo


# TODO 或许能加快下载速度的方法（https://gist.github.com/painor/7e74de80ae0c819d3e9abcf9989a8dd6）


def file_exist(file_path: str, file_size: int) -> tuple[bool, str]:
    """检查文件是否存在（文件名和大小都相等），若存在重名文件则自动加序号。"""
    i = 2
    file_name, file_type = os.path.splitext(file_path)
    temp = file_path
    while os.path.exists(temp):
        if os.path.getsize(temp) == file_size:
            return True, temp
        temp = f'{file_name}({i}){file_type}'
        i += 1
    return False, temp


def get_file_suffix(message) -> list[str]:
    mime_type = 'unknown/unknown'
    if hasattr(message.media, 'document'):
        mime_type = message.media.document.mime_type
    elif hasattr(message.media, 'photo'):
        mime_type = 'image/jpg'
    return mime_type.split('/')


async def download_file(client: TelegramClient, channel_title, channel_id, message, prefix=None):
    message_time = message.date
    formatted_time = datetime.strftime(message_time, '%Y_%m')

    file_name = get_file_name(message)
    if not match_wildcard(prefix, file_name):
        return
    file_path = os.path.join(os.environ["save_path"], f'{channel_title}-{channel_id}', file_name)
    file_size = message.file.size
    ret, file_path = file_exist(file_path, file_size)
    if ret:
        print(f"媒体已存在：{file_path}")
        return

    download_path = file_path + '.downloading'
    print(f"开始下载：{file_name}")

    for attempt in range(2):  # 最多尝试 2 次
        try:
            from asyncio import CancelledError
            with TqdmUpTo(total=file_size, bar_format=TqdmUpTo.bar_format, desc=file_name[:10]) as bar:
                await message.download_media(download_path, progress_callback=bar.update_to)
            os.rename(download_path, file_path)
            return
        except CancelledError:
            print("取消下载")
            os.remove(download_path)
            import sys
            sys.exit()
        except FileReferenceExpiredError:
            if attempt == 1:
                print('重试失败，跳过该文件')
                return
            print('下载超时，重试中')
            channelData = await client.get_entity(int(channel_id))
            newMessages = client.iter_messages(entity=channelData, ids=message.id)
            async for newMessage in newMessages:
                message = newMessage
                break
        except Exception as e:
            print("下载出错", e.__class__.__name__)
            os.remove(download_path)
            return


async def down_group(client: TelegramClient, chat_id, plus_func: str, from_user, prefix):
    chat_id = await get_chat_id(client, chat_id)
    target_user_ids = await parse_user_ids(client, from_user)
    channel_title, messages = await get_history_message(client, chat_id, plus_func, from_user_ids=target_user_ids)
    async for message in messages:
        if message is None:
            print('慢了一步，消息已被删除')
            continue

        if not isinstance(message.media, (MessageMediaDocument, MessageMediaPhoto)):
            continue

        if isinstance(message.media, MessageMediaDocument) and any(
                isinstance(attr, DocumentAttributeSticker) for attr in message.media.document.attributes):
            continue

        await download_file(
            client=client,
            channel_title=channel_title,
            channel_id=chat_id,
            message=message,
            prefix=prefix
        )
    print(channel_title, '全部下载完成')