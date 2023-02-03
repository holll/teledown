import os
import re

import demoji
from telethon import TelegramClient
from telethon.tl import types

import tools.tool
from tools.tool import shorten_filename
from tools.tqdm import TqdmUpTo


# TODO 或许能加快下载速度的方法（https://gist.github.com/painor/7e74de80ae0c819d3e9abcf9989a8dd6）
async def download_file(channel_title, channel_id, message):
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
    # channel_title += '/'
    message.message = shorten_filename(message.message)
    if is_photo:
        file_type = 'jpg'
    else:
        file_type = message.media.document.mime_type.split('/')[-1]
    file_name = message.message + f'.{file_type}'
    # long_path = os.environ['save_path'] + channel_title + file_name
    long_path = f'{os.environ["save_path"]}/{channel_title}-{channel_id}/{message.message}.{file_type}'
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
    # 检测chat_id是id还是昵称
    isId = re.match(r'-?[1-9][0-9]{4,}', chat_id)
    if isId is None:
        entity = await client.get_entity(chat_id)
        chat_id = entity.id
    channel_title, messages = await tools.tool.getHistoryMessage(client, int(chat_id))
    # 频道名称中的表情转文字，以兼容不同字符集设备
    channel_title = demoji.replace(channel_title, '[emoji]')
    # Todo 为了应对某些频道改名导致存储路径更新，通过chat_id预处理文件夹名称
    # 识别到存在相同id文件夹时，更新旧文件夹名称
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
            await download_file(channel_title, chat_id, message)
    print(channel_title, '全部下载完成')
