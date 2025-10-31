import fnmatch
import hashlib
import os
import re
import subprocess
import sys
from io import BytesIO
from typing import Union

import demoji
import pandas as pd
from telethon import TelegramClient
from telethon.tl import types


def shorten_filename(filename, limit=50):
    filename = filename.replace('\n', ' ')
    """返回合适长度文件名，中间用...显示"""
    if len(filename) <= limit:
        return filename
    else:
        return filename[:int(limit / 2) - 3] + '...' + filename[len(filename) - int(limit / 2):]


def print_all_channel(client: TelegramClient):
    Ids = []
    Names = []
    # Types = []
    for d in client.iter_dialogs():
        if not isinstance(d.entity, types.Channel):
            continue
        Ids.append(d.entity.id)
        Names.append(d.name)
        # Types.append(d.entity)

    df = pd.DataFrame({'ID': Ids, '频道名': Names})
    df.sort_values("频道名", inplace=True)
    df.to_csv('全部频道.csv', index=False)
    print('全部输出完成')


async def getHistoryMessage(client: TelegramClient, chat_id: int, plus_func=Union[None, str], from_user=None):
    channel_title = await GetChatTitle(client, chat_id)
    if from_user is not None and from_user.isdecimal():
        from_user = int(from_user)
    # Todo 根据plus_func获取指定消息区间
    if plus_func is not None:
        filterFunc = plus_func[:1]
        # 多选消息ID模式
        if ',' in plus_func:
            ids = [int(_id) for _id in plus_func.split(',')]
            messages = client.iter_messages(chat_id, reverse=True, min_id=1, ids=ids)
        elif filterFunc != 's':
            specifyID = int(plus_func[1:])
            # 大于范围模式
            if filterFunc == '>':
                messages = client.iter_messages(chat_id, min_id=specifyID, from_user=from_user)
            # 小于范围模式
            elif filterFunc == '<':
                messages = client.iter_messages(chat_id, max_id=specifyID, from_user=from_user)
            else:
                # 单选模式
                messages = client.iter_messages(chat_id, ids=specifyID)
        else:
            # 区间模式
            tmpId = plus_func[1:].split('s')
            messages = client.iter_messages(chat_id, max_id=int(tmpId[-1]), min_id=int(tmpId[0]), from_user=from_user)
    else:
        messages = client.iter_messages(chat_id, reverse=True, min_id=1, from_user=from_user)
    return channel_title, messages


async def GetChatId(client: TelegramClient, chat_id: str) -> int:
    # 检测chat_id是id还是昵称
    isId = re.match(r'-?[1-9][0-9]{4,}', chat_id)
    if isId is None:
        entity = await client.get_entity(chat_id)
        chat_id = entity.id
    else:
        chat_id = int(chat_id)
    return chat_id


async def GetChatTitle(client: TelegramClient, chat_id: int) -> Union[str, None]:
    if os.environ.get(str(chat_id)):
        return os.environ.get(str(chat_id))
    entity = await client.get_entity(chat_id)
    if isinstance(entity, types.User):
        title = f'{entity.username}({entity.first_name + str(entity.last_name)})'
    elif isinstance(entity, types.Channel):
        title = entity.title
    elif isinstance(entity, types.Chat):
        title = ''
        pass
    else:
        return None
    title = re.sub(r'[\\/:*?"<>|]', '', demoji.replace(title, ''))
    return title


def GetFileId(message) -> str:
    _id = 'unknown'
    if hasattr(message.media, 'document'):
        _id = message.media.document.id
    elif hasattr(message.media, 'photo'):
        _id = message.media.photo.id
    return str(_id)


def GetFileName(message) -> str:
    # 取名优先级，文件名>描述>ID
    if message.file.name:
        return message.file.name

    file_ext = '.jpg' if message.file.ext in ['.jpe', 'jpeg'] else message.file.ext
    if len(message.message) != 0:
        sName = shorten_filename(demoji.replace(message.message, '[emoji]'))
        return re.sub(r'[\\/:*?"<>|]', '_', sName) + file_ext
    return GetFileId(message) + file_ext


async def print_group(client: TelegramClient, chat_id):
    # 检测chat_id是id还是昵称
    isId = re.match(r'-?[1-9][0-9]{4,}', chat_id)
    if isId is None:
        entity = await client.get_entity(chat_id)
        chat_id = entity.id
    channel_title, messages = await getHistoryMessage(client=client, chat_id=int(chat_id))  # messages是倒序的
    channel_title = demoji.replace(channel_title, '[emoji]')
    channel_title = re.sub(r'[\\/:*?"<>|]', '', channel_title)
    links = []
    names = []
    submits = []
    sizes = []
    async for message in messages:
        if message.media is not None:
            # 获取文件类型
            is_photo = isinstance(message.media, types.MessageMediaPhoto)
            is_doc = isinstance(message.media, types.MessageMediaDocument)
            if not (is_photo or is_doc):
                continue

            file_name = GetFileName(message)

            file_size = message.file.size
            file_size = f'{round(file_size / 1024 ** 2, 2)}MB' if file_size > 1024 ** 2 else f'{round(file_size / 1024, 2)}KB'
            link = f'https://t.me/c/{chat_id}/{message.id}'
            links.append(link)
            names.append(file_name)
            submits.append(message.message)
            sizes.append(file_size)

    df = pd.DataFrame({'链接': links, '文件名': names, '描述': submits, '大小': sizes})
    # 去重
    df.drop_duplicates(subset=["文件名", "大小"], keep="first", inplace=True)
    df.sort_values("链接", inplace=True, ascending=False)
    df.to_csv(f'{channel_title}-{chat_id}.csv', index=False)
    # df.to_csv(f'{os.environ["save_path"]}/{channel_title}-{chat_id}/{chat_id}.csv', index=False)
    print(chat_id, '全部输出完成')


# 确保数据库没有被占用
def initDb(md5Token):
    # 获取当前 Python 文件所在的目录路径
    current_dir_path = os.path.dirname(os.path.abspath(__file__))
    # 获取项目根目录的路径
    root_dir_path = os.path.join(current_dir_path, "..")
    root_abspath = os.path.abspath(root_dir_path)
    if sys.platform == 'linux':
        # 检测的文件路径
        file_path = os.path.join(root_abspath, f"{md5Token}.session")
        if not os.path.exists(file_path):
            return
            # 查询文件占用情况
        p1 = subprocess.Popen(["lsof", file_path], stdout=subprocess.PIPE)
        output, _ = p1.communicate()
        # lsof 命令返回非零值表示文件被占用
        if p1.returncode == 0:
            print("数据库文件被占用，释放中")
            # 查询文件占用情况
            p1 = subprocess.Popen(["lsof", "-t", file_path], stdout=subprocess.PIPE)
            # 读取子进程的 stdout 输出并等待子进程结束
            stdout, _ = p1.communicate()
            # 将 stdout 的结果（即进程 ID）作为参数传递给 kill 命令，结束该进程
            p2 = subprocess.Popen(["kill", "-9", stdout.strip().decode()])
            p2.wait(timeout=5)
    else:
        return


def get_all_files(path):
    all_files = []
    for root, dirs, files in os.walk(path):
        for filename in files:
            # 获取文件的绝对路径
            filepath = os.path.join(root, filename)
            all_files.append(filepath)
    return sorted(all_files)


def GetThumb(file_path: str) -> bytes:
    from PIL import Image
    from moviepy.video.io.VideoFileClip import VideoFileClip
    # 打开视频文件并获取第一帧图像
    with VideoFileClip(file_path) as video:
        # 获取视频中指定时间的图像
        video_image = video.get_frame(0)

    # 将图像转换为 PIL Image 对象
    video_image = Image.fromarray(video_image)

    # 将图像数据保存为 BytesIO 对象
    thumb_bytes_io = BytesIO()
    video_image.save(thumb_bytes_io, format='JPEG')
    thumb_bytes = thumb_bytes_io.getvalue()

    return thumb_bytes


def md5(string):
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
        file_type = magic_obj.from_buffer(file_content)
        return file_type


def match_wildcard(pattern, string):
    if pattern is None:
        return True
    patterns = pattern.split(";")
    return any(fnmatch.fnmatch(string, pat) for pat in patterns)


async def Hook(client: TelegramClient):
    return
