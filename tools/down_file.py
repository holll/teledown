import os
import re

import demoji
import pandas as pd
from telethon import TelegramClient
from telethon.tl import types

import tools.tool
from tools.tool import shorten_filename
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


# 获取服务器的文件大小
def getFileSize(message, is_doc: bool, is_photo: bool):
    file_size = 0
    if is_doc:
        file_size = message.media.document.size
    elif is_photo:
        if hasattr(message.media.photo.sizes[-1], 'sizes'):
            file_size = message.media.photo.sizes[-1].sizes[-1]
        if hasattr(message.media.photo.sizes[-1], 'size'):
            file_size = message.media.photo.sizes[-1].size
    return file_size


def GetFileName(message, hasRawName: bool) -> str:
    if hasRawName:
        return message.media.document.attributes[-1].file_name
    else:
        # 如果文件有文字说明，则用文件说明来命名
        if len(message.message) != 0:
            sName = shorten_filename(demoji.replace(message.message, '[emoji]'))
            return re.sub(r'[\\/:*?"<>|]', '_', sName)
        # 否则用消息id来命名
        else:
            return str(message.photo.id) + '.jpg'


async def download_file(channel_title, channel_id, message):
    # 获取媒体类型
    is_photo = isinstance(message.media, types.MessageMediaPhoto)
    is_doc = isinstance(message.media, types.MessageMediaDocument)

    # 如果不是文件就放弃（可能是音频文字啥的）
    if not (is_photo or is_doc):
        return
    hasRawName = hasattr(message.media.document.attributes[-1], 'file_name')
    file_name = GetFileName(message, hasRawName)
    file_path = f'{os.environ["save_path"]}/{channel_title}-{channel_id}/{file_name}'
    file_size = getFileSize(message, is_doc=is_doc, is_photo=is_photo)
    ret, file_path = fileExist(file_path, file_size)
    if not ret:
        # 已经判断文件不存在，并且保证了文件名不重复
        download_path = file_path + '.downloading'
        print(f"开始下载：{file_name}")
        try:
            with TqdmUpTo(unit='B', unit_scale=True, unit_divisor=1024, total=file_size,
                          bar_format=TqdmUpTo.bar_format, desc=message.message[:10]) as bar:
                await message.download_media(download_path, progress_callback=bar.update_to)
        except Exception as e:
            print("下载出错", e.__class__.__name__)
            os.remove(download_path)
        else:
            os.rename(download_path, file_path)
    else:
        print(f"媒体已存在：{file_path}")


async def down_group(client: TelegramClient, chat_id, plus_func: str):
    # 检测chat_id是id还是昵称
    isId = re.match(r'-?[1-9][0-9]{4,}', chat_id)
    if isId is None:
        entity = await client.get_entity(chat_id)
        chat_id = entity.id
    channel_title, messages = await tools.tool.getHistoryMessage(client, int(chat_id))  # messages是倒序的
    # 频道名称中的表情转文字，以兼容不同字符集设备
    channel_title = demoji.replace(channel_title, '[emoji]')
    channel_title = re.sub(r'[\\/:*?"<>|]', '', channel_title)
    # Todo 为了应对某些频道改名导致存储路径更新，通过chat_id预处理文件夹名称
    # 识别到存在相同id文件夹时，更新旧文件夹名称
    async for message in messages:
        # 0表示执行下载操作，1表示跳过消息，2表示break
        switch = tools.tool.can_continue(message.id, plus_func)
        if switch == 1:
            continue
        elif switch == 2:
            break
        if message.media is not None:
            await download_file(channel_title, chat_id, message)
    print(channel_title, '全部下载完成')


async def print_group(client: TelegramClient, chat_id, plus_func: str):
    # 检测chat_id是id还是昵称
    isId = re.match(r'-?[1-9][0-9]{4,}', chat_id)
    if isId is None:
        entity = await client.get_entity(chat_id)
        chat_id = entity.id
    channel_title, messages = await tools.tool.getHistoryMessage(client, int(chat_id))  # messages是倒序的
    channel_title = demoji.replace(channel_title, '[emoji]')
    channel_title = re.sub(r'[\\/:*?"<>|]', '', channel_title)
    links = []
    names = []
    sizes = []
    async for message in messages:
        switch = tools.tool.can_continue(message.id, plus_func)
        if switch == 1:
            continue
        elif switch == 2:
            break
        if message.media is not None:
            # 获取文件类型
            is_photo = isinstance(message.media, types.MessageMediaPhoto)
            is_doc = isinstance(message.media, types.MessageMediaDocument)
            if not (is_photo or is_doc):
                continue

            if len(message.message) != 0:
                message.message = demoji.replace(message.message, '[emoji]')
            # 否则用消息id来命名
            else:
                if is_photo:
                    message.message = str(message.photo.id)
                else:
                    message.message = str(message.document.id)
            message.message = shorten_filename(message.message)
            if is_photo:
                file_type = 'jpg'
            else:
                file_type = message.media.document.mime_type.split('/')[-1]
                if file_type == "quicktime":
                    file_type = "mov"
                if hasattr(message.media.document.attributes[-1], 'file_name'):
                    message.message = message.media.document.attributes[-1].file_name.rsplit('.', 1)[0]
            file_name = message.message + '.' + file_type
            file_name = re.sub(r'[\\/:*?"<>|]', '_', file_name)

            file_size = getFileSize(message, is_doc=is_doc, is_photo=is_photo)
            file_size = f'{round(file_size / 1024 ** 2, 2)}MB' if file_size > 1024 ** 2 else f'{round(file_size / 1024, 2)}KB'
            link = f'https://t.me/c/{chat_id}/{message.id}'
            links.append(link)
            names.append(file_name)
            sizes.append(file_size)

    df = pd.DataFrame({'链接': links, '文件名': names, '大小': sizes})
    # 去重
    df.drop_duplicates(subset=["文件名", "大小"], keep="first", inplace=True)
    df.sort_values("文件名", inplace=True)
    #    df.to_csv(f'{chat_id}.csv',index=False)
    df.to_csv(f'{os.environ["save_path"]}/{channel_title}-{chat_id}/{chat_id}.csv', index=False)
    print(chat_id, '全部输出完成')
