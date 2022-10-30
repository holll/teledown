import os

from telethon import TelegramClient
from telethon.tl import types

import tools.tool
from tools.tool import shorten_filename
from tools.tqdm import TqdmUpTo


async def download_file(channel_title, message):
    # 获取媒体类型
    is_webpage = isinstance(message.media, types.MessageMediaWebPage)
    is_photo = isinstance(message.media, types.MessageMediaPhoto)
    is_doc = isinstance(message.media, types.MessageMediaDocument)

    # 如果不是文件就放弃（可能是音频啥的）
    if not (is_photo or is_doc):
        return
    # download_media()可以自动命名，下载成功后会返回文件的保存名
    if len(message.message) == 0:
        if is_photo:
            message.message = str(message.photo.id)
        else:
            message.message = str(message.document.id)
    channel_title += '/'
    message.message = shorten_filename(message.message)
    if is_photo:
        file_type = 'jpg'
    else:
        file_type = message.media.document.mime_type.split('/')[-1]
    file_name = message.message + f'.{file_type}'
    long_path = os.environ['save_path'] + channel_title + file_name
    if not os.path.exists(long_path) or (is_doc and os.path.getsize(long_path) != message.media.document.size) or (
            is_photo and hasattr(message.media.photo.sizes, 'sizes') and os.path.getsize(long_path) !=
            message.media.photo.sizes[-1].sizes[-1] or is_photo and hasattr(message.media.photo.sizes,
                                                                            'size') and os.path.getsize(long_path) !=
            message.media.photo.sizes[-1].size):
        print(f"开始下载：{file_name}")
        with TqdmUpTo(unit='B', unit_scale=True, unit_divisor=1024,
                      bar_format=TqdmUpTo.bar_format, desc=message.message[:10]) as t:
            filename = await message.download_media(long_path, progress_callback=t.update_to)
        # filename = await message.download_media(save_path + message.message, progress_callback=callback)
        # print(f"媒体下载完成{filename}")
    else:
        print(f"媒体已存在：{long_path}")
    # 下面注释的代码不知道什么原因无法在文件不存在的情况下新建文件
    # async with async_open(save_path + "1.txt", "a") as f:
    #     await f.write(filename + "\n")
    # 原消息内容输出显示
    # print(message.sender.id, message.raw_text)


async def down_group(client: TelegramClient, chat_id, plus_func: str):
    channel_title, messages = await tools.tool.getHistoryMessage(client, int(chat_id))
    async for message in messages:
        # 0表示不执行操作，1表示continue，2表示break
        switch = tools.tool.can_continue(message.id, plus_func)
        if switch == 1:
            continue
        elif switch == 2:
            break
        if message.media is not None:
            # 下载媒体
            # print(f'开始转发{message.message}')
            # message.message += f' #{chat_id}'

            # await client.send_message(-1001877389961, message)
            # await message.forward_to(-1001877389961)
            await download_file(channel_title, message)
    print(channel_title, '全部下载完成')
