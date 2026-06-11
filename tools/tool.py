import fnmatch
import hashlib
import os
import re
import subprocess
import sys
from io import BytesIO
from typing import Optional

import demoji
import pandas as pd
from telethon import TelegramClient
from telethon.tl import types


# 模块级频道别名缓存，替代 os.environ 做缓存
_chat_title_cache: dict[str, str] = {}


def set_chat_alias(alias_map: dict[str, str]) -> None:
    """设置频道别名缓存（由 main.py 在启动时调用）。"""
    _chat_title_cache.update(alias_map)


def shorten_filename(filename: str, limit: int = 50) -> str:
    """返回合适长度文件名，中间用...显示"""
    filename = filename.replace('\n', ' ')
    if len(filename) <= limit:
        return filename
    return filename[:int(limit / 2) - 3] + '...' + filename[len(filename) - int(limit / 2):]


async def print_all_channel(client: TelegramClient) -> None:
    ids = []
    names = []
    async for d in client.iter_dialogs():
        if not isinstance(d.entity, types.Channel):
            continue
        ids.append(d.entity.id)
        names.append(d.name)

    df = pd.DataFrame({'ID': ids, '频道名': names})
    df.sort_values("频道名", inplace=True)
    df.to_csv('全部频道.csv', index=False)
    print('全部输出完成')


async def parse_user_ids(client: TelegramClient, user_refs) -> set[int]:
    target_ids: set[int] = set()
    if not user_refs:
        return target_ids

    for user_ref in str(user_refs).replace('|', ',').split(','):
        user_ref = user_ref.strip()
        if not user_ref:
            continue
        if user_ref.isdecimal():
            target_ids.add(int(user_ref))
            continue
        entity = await client.get_entity(user_ref)
        target_ids.add(entity.id)
    return target_ids


async def get_history_message(
    client: TelegramClient,
    chat_id: int,
    plus_func: Optional[str] = None,
    from_user_ids: Optional[set[int]] = None,
):
    channel_title = await get_chat_title(client, chat_id)
    filter_user = None
    if from_user_ids:
        if len(from_user_ids) == 1:
            filter_user = next(iter(from_user_ids))

    async def filter_messages_by_user(messages):
        async for message in messages:
            if not from_user_ids or message.sender_id in from_user_ids:
                yield message

    if plus_func is not None:
        # 自动检测 .. 分隔符并标准化为 s 前缀格式，兼容 --range "10..200" 用法
        if '..' in plus_func and plus_func[0] not in ('>', '<', '=', 's'):
            plus_func = 's' + plus_func
        filterFunc = plus_func[:1]
        # 多选消息ID模式
        if ',' in plus_func:
            ids = [int(_id) for _id in plus_func.split(',')]
            messages = client.iter_messages(chat_id, reverse=True, min_id=1, ids=ids)
        elif filterFunc != 's':
            specifyID = int(plus_func[1:])
            # 大于范围模式
            if filterFunc == '>':
                messages = client.iter_messages(chat_id, min_id=specifyID, from_user=filter_user)
            # 小于范围模式
            elif filterFunc == '<':
                messages = client.iter_messages(chat_id, max_id=specifyID, from_user=filter_user)
            else:
                # 单选模式
                messages = client.iter_messages(chat_id, ids=specifyID)
        else:
            # 区间模式 — 支持 s 和 .. 两种分隔符
            rest = plus_func[1:]
            if '..' in rest:
                low, high = rest.split('..', 1)
            else:
                low, high = rest.split('s', 1)
            messages = client.iter_messages(chat_id, max_id=int(high), min_id=int(low), from_user=filter_user)
    else:
        messages = client.iter_messages(chat_id, reverse=True, min_id=1, from_user=filter_user)

    return channel_title, filter_messages_by_user(messages)


async def get_chat_id(client: TelegramClient, chat_id: str) -> int:
    """检测 chat_id 是 ID 还是昵称，返回整数 ID。"""
    isId = re.match(r'-?[1-9][0-9]{4,}', chat_id)
    if isId is None:
        entity = await client.get_entity(chat_id)
        chat_id = entity.id
    else:
        chat_id = int(chat_id)
    return chat_id


async def get_chat_title(client: TelegramClient, chat_id: int) -> Optional[str]:
    """获取频道/用户标题，优先使用别名缓存。"""
    cache_key = str(chat_id)
    if cache_key in _chat_title_cache:
        return _chat_title_cache[cache_key]

    entity = await client.get_entity(chat_id)
    if isinstance(entity, types.User):
        first = entity.first_name or ''
        last = entity.last_name or ''
        title = f'{entity.username}({first}{last})'
    elif isinstance(entity, types.Channel):
        title = entity.title
    elif isinstance(entity, types.Chat):
        title = entity.title
    else:
        return None
    title = re.sub(r'[\\/:*?"<>|]', '', demoji.replace(title, ''))
    return title


def get_file_id(message) -> str:
    _id = 'unknown'
    if hasattr(message.media, 'document'):
        _id = message.media.document.id
    elif hasattr(message.media, 'photo'):
        _id = message.media.photo.id
    return str(_id)


def get_file_name(message) -> str:
    """从消息中提取文件名，优先级：原始文件名 > 消息文本描述 > 文件 ID。"""
    if message.file.name:
        return message.file.name

    # 统一 JPEG 变体后缀
    file_ext = message.file.ext
    if file_ext in ('.jpe', '.jpeg'):
        file_ext = '.jpg'

    if len(message.message) != 0:
        sName = shorten_filename(demoji.replace(message.message, '[emoji]'))
        return re.sub(r'[\\/:*?"<>|]', '_', sName) + file_ext
    return get_file_id(message) + file_ext


async def print_group(client: TelegramClient, chat_id) -> None:
    """打印频道消息到 CSV。"""
    isId = re.match(r'-?[1-9][0-9]{4,}', chat_id)
    if isId is None:
        entity = await client.get_entity(chat_id)
        chat_id = entity.id
    channel_title, messages = await get_history_message(client=client, chat_id=int(chat_id))
    channel_title = demoji.replace(channel_title, '[emoji]')
    channel_title = re.sub(r'[\\/:*?"<>|]', '', channel_title)
    links: list[str] = []
    names: list[str] = []
    submits: list[str] = []
    sizes: list[str] = []
    async for message in messages:
        if message.media is not None:
            is_photo = isinstance(message.media, types.MessageMediaPhoto)
            is_doc = isinstance(message.media, types.MessageMediaDocument)
            if not (is_photo or is_doc):
                continue

            file_name = get_file_name(message)

            file_size = message.file.size
            file_size = (
                f'{round(file_size / 1024 ** 2, 2)}MB'
                if file_size > 1024 ** 2
                else f'{round(file_size / 1024, 2)}KB'
            )
            link = f'https://t.me/c/{chat_id}/{message.id}'
            links.append(link)
            names.append(file_name)
            submits.append(message.message)
            sizes.append(file_size)

    df = pd.DataFrame({'链接': links, '文件名': names, '描述': submits, '大小': sizes})
    df.drop_duplicates(subset=["文件名", "大小"], keep="first", inplace=True)
    df.sort_values("链接", inplace=True, ascending=False)
    df.to_csv(f'{channel_title}-{chat_id}.csv', index=False)
    print(chat_id, '全部输出完成')


def init_db(md5_token: str) -> None:
    """检查 session 文件是否被占用（仅 Linux），若占用则给出警告。"""
    current_dir_path = os.path.dirname(os.path.abspath(__file__))
    root_dir_path = os.path.join(current_dir_path, "..")
    root_abspath = os.path.abspath(root_dir_path)
    if sys.platform == 'linux':
        file_path = os.path.join(root_abspath, f"{md5_token}.session")
        if not os.path.exists(file_path):
            return
        p1 = subprocess.Popen(["lsof", file_path], stdout=subprocess.PIPE)
        _output, _ = p1.communicate()
        if p1.returncode == 0:
            print("警告：session 文件被其他进程占用，请关闭其他 teledown 实例后重试")


def get_all_files(path: str) -> list[str]:
    all_files: list[str] = []
    for root, dirs, files in os.walk(path):
        for filename in files:
            filepath = os.path.join(root, filename)
            all_files.append(filepath)
    return sorted(all_files)


def get_thumb(file_path: str) -> bytes:
    """从视频文件提取第一帧作为缩略图。"""
    from PIL import Image
    from moviepy.video.io.VideoFileClip import VideoFileClip

    with VideoFileClip(file_path) as video:
        video_image = video.get_frame(0)

    video_image = Image.fromarray(video_image)
    thumb_bytes_io = BytesIO()
    video_image.save(thumb_bytes_io, format='JPEG')
    return thumb_bytes_io.getvalue()


def md5(string: str) -> str:
    m = hashlib.md5()
    m.update(string.encode('utf-8'))
    return m.hexdigest()


def str2join(*args) -> str:
    content = ''
    for char in args:
        if char is None:
            continue
        content += str(char)
    return content


def get_filetype(path: str) -> str:
    import magic
    magic_obj = magic.Magic(mime=True)
    with open(path, 'rb') as f:
        file_content = f.read(1024)
        return magic_obj.from_buffer(file_content)


def match_wildcard(pattern: Optional[str], string: str) -> bool:
    if pattern is None:
        return True
    patterns = pattern.split(";")
    return any(fnmatch.fnmatch(string, pat) for pat in patterns)


async def run_hook(client: TelegramClient) -> None:
    """自定义 Hook 占位函数。"""
    return