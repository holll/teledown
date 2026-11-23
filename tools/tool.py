import csv
import fnmatch
import hashlib
import os
import re
import shutil
import subprocess
import sys
from collections.abc import Iterator
from contextlib import contextmanager
from io import BytesIO
from pathlib import Path
from typing import Optional

import demoji
from telethon import TelegramClient, utils
from telethon.tl import types

# 模块级频道别名缓存，替代 os.environ 做缓存
_chat_title_cache: dict[str, str] = {}


class UserAbort(Exception):
    """用户在下载/上传过程中主动中断。

    在协程内直接调用 sys.exit() 会让 run_until_complete 抛出 SystemExit 并以退出码 0 结束，
    无法与正常成功区分；改为抛本异常，由 main.py 统一以非 0 状态退出。
    """


def set_chat_alias(alias_map: dict[str, str]) -> None:
    """设置频道别名缓存（由 main.py 在启动时调用）。"""
    _chat_title_cache.update(alias_map)


def shorten_filename(filename: str, limit: int = 50) -> str:
    """返回合适长度文件名，中间用...显示"""
    filename = filename.replace('\n', ' ')
    if len(filename) <= limit:
        return filename
    return filename[:int(limit / 2) - 3] + '...' + filename[len(filename) - int(limit / 2):]


def _write_csv(path: str, headers: tuple[str, ...], rows) -> None:
    with open(path, 'w', encoding='utf-8', newline='') as file:
        writer = csv.writer(file, lineterminator=os.linesep)
        writer.writerow(headers)
        writer.writerows(rows)


async def print_all_channel(client: TelegramClient) -> None:
    rows = []
    async for dialog in client.iter_dialogs():
        if isinstance(dialog.entity, types.Channel):
            rows.append((dialog.entity.id, dialog.name))

    rows.sort(key=lambda row: (row[1] is None, row[1] or ''))
    _write_csv('全部频道.csv', ('ID', '频道名'), rows)
    print('全部输出完成')


async def parse_user_ids(client: TelegramClient, user_refs: Optional[str]) -> Optional[set[int]]:
    """None 表示未指定过滤；显式列表不允许有无效项。"""
    if user_refs is None:
        return None

    refs = [part.strip() for part in user_refs.replace('|', ',').split(',')]
    if not all(refs):
        raise UserAbort('-user 不能为空，也不能包含空的用户标识')

    target_ids: set[int] = set()
    for user_ref in refs:
        if re.fullmatch(r'-?[0-9]+', user_ref):
            user_id = int(user_ref)
            if not user_id:
                raise UserAbort('-user 的用户 ID 不能为 0')
            target_ids.add(user_id)
            continue
        try:
            entity = await client.get_entity(user_ref)
            target_ids.add(utils.get_peer_id(entity))
        except Exception as exc:
            raise UserAbort(f'无法解析用户 {user_ref}: {exc}') from exc
    return target_ids


def matches_sender(sender_id: Optional[int], user_ids: Optional[set[int]]) -> bool:
    return user_ids is None or (sender_id is not None and sender_id in user_ids)


def parse_message_range(value: str) -> dict:
    """把 CLI 消息范围转换为 Telethon 的查询参数。"""
    max_message_id = 2_147_483_647

    def message_id(text: str, minimum: int = 1) -> int:
        number = int(text)
        if not minimum <= number <= max_message_id:
            raise ValueError(f'消息 ID 必须在 {minimum} 到 {max_message_id} 之间')
        return number

    value = value.strip()
    if re.fullmatch(r's[0-9]+(?:s|\.\.)[0-9]+', value):
        raise ValueError('旧区间格式已移除，请改用 A..B（例如 1..100）')
    if match := re.fullmatch(r'([0-9]+)\.\.([0-9]+)', value):
        low, high = message_id(match[1], 0), message_id(match[2])
        if low > high:
            raise ValueError('区间下限不能大于上限')
        # Telethon 的上下界均为开区间；reverse=True 从最小 ID 开始分页。
        return {'min_id': max(0, low - 1), 'max_id': 0 if high == max_message_id else high + 1, 'reverse': True}
    if match := re.fullmatch(r'([<>])([0-9]+)', value):
        if match[1] == '>':
            low = message_id(match[2], 0)
            if low == max_message_id:
                # 空区间；避免 Telethon 把下限加一后作为 int32 游标发送。
                return {'min_id': low, 'max_id': low, 'reverse': True}
            return {'min_id': low, 'reverse': True}
        return {'max_id': message_id(match[2]), 'reverse': True}
    if match := re.fullmatch(r'=?([0-9]+)', value):
        return {'ids': message_id(match[1])}
    if re.fullmatch(r'[0-9]+(?:\s*,\s*[0-9]+)+', value):
        # Telethon 按请求顺序取指定 ID；reverse=True 会反转列表，而不会排序。
        return {'ids': sorted(message_id(part) for part in value.split(','))}
    raise ValueError('应为 >N、<N、N、=N、A..B 或逗号分隔的消息 ID')


async def get_history_message(
        client: TelegramClient,
        chat_id: int,
        message_range: Optional[dict] = None,
        from_user_ids: Optional[set[int]] = None,
):
    channel_title = await get_chat_title(client, chat_id)
    async def filter_messages_by_user(messages):
        async for message in messages:
            if message is None or matches_sender(message.sender_id, from_user_ids):
                yield message

    query = dict(message_range) if message_range is not None else {'reverse': True, 'min_id': 1}
    if 'min_id' in query and 'max_id' in query and from_user_ids and len(from_user_ids) == 1:
        query['from_user'] = next(iter(from_user_ids))
    messages = client.iter_messages(chat_id, **query)

    return channel_title, filter_messages_by_user(messages)


async def get_chat_id(client: TelegramClient, chat_id) -> int:
    """检测 chat_id 是 ID 还是昵称，返回整数 ID。

    同时兼容 str 与 int：调用方可能已经把纯数字 ID 转成 int
    （argparse 的 type=int、配置项解析、监控/钩子回调等），
    旧实现直接对 int 调 re.match() 会抛 TypeError。
    """
    if chat_id is None or isinstance(chat_id, bool):
        raise TypeError(f'get_chat_id 需要 str 或 int，收到 {type(chat_id).__name__}')
    if isinstance(chat_id, int):
        return chat_id
    text = str(chat_id).strip()
    if not text:
        raise ValueError('get_chat_id 收到空的频道标识')
    # 纯数字（可带负号）视为 ID；fullmatch 避免 "12345abc" 这类前缀被误判成 ID
    if re.fullmatch(r'-?[1-9][0-9]{4,}', text):
        return int(text)
    entity = await client.get_entity(text)
    return entity.id


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
    if message.file and message.file.name:
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
    rows = []
    seen: set[tuple[str, str]] = set()
    async for message in messages:
        if message is None or not isinstance(message.media, (types.MessageMediaPhoto, types.MessageMediaDocument)):
            continue

        file_name = get_file_name(message)
        size = message.file.size
        formatted_size = (
            f'{round(size / 1024 ** 2, 2)}MB'
            if size > 1024 ** 2
            else f'{round(size / 1024, 2)}KB'
        )
        key = (file_name, formatted_size)
        if key in seen:
            continue
        seen.add(key)
        link = f'https://t.me/c/{chat_id}/{message.id}'
        rows.append((link, file_name, message.message, formatted_size))

    rows.sort(key=lambda row: row[0], reverse=True)
    _write_csv(f'{channel_title}-{chat_id}.csv', ('链接', '文件名', '描述', '大小'), rows)
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


def configure_ffmpeg() -> None:
    """在 MoviePy 加载配置前，只选择外部 FFmpeg。"""
    override = os.environ.get('FFMPEG_BINARY')
    executable = shutil.which(override or 'ffmpeg')
    if executable is None:
        hint = '，请检查 FFMPEG_BINARY' if override else '，请安装 FFmpeg 并将其加入 PATH，或设置 FFMPEG_BINARY'
        raise UserAbort(f'找不到可执行的 FFmpeg{hint}')
    os.environ['FFMPEG_BINARY'] = str(Path(executable).absolute())


@contextmanager
def external_ffmpeg() -> Iterator[None]:
    """让 MoviePy 启动的 FFmpeg 使用打包程序启动前的系统库路径。"""
    configure_ffmpeg()
    if sys.platform != 'linux' or not getattr(sys, 'frozen', False):
        yield
        return

    key = 'LD_LIBRARY_PATH'
    bundled_path = os.environ.get(key)
    original_path = os.environ.get(f'{key}_ORIG')
    # MoviePy 不提供子进程 env 参数。仅在同步调用期间切换，不跨 await。
    if original_path:
        os.environ[key] = original_path
    else:
        os.environ.pop(key, None)
    try:
        yield
    finally:
        if bundled_path is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = bundled_path


def get_thumb(file_path: str) -> bytes:
    """从视频文件提取第一帧作为缩略图。"""
    with external_ffmpeg():
        from PIL import Image
        from moviepy import VideoFileClip

        with VideoFileClip(file_path, audio=False) as video:
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
    magic_database = None
    if sys.platform == 'linux' and getattr(sys, 'frozen', False):
        magic_database = str(Path(sys._MEIPASS) / 'teledown-magic' / 'magic.mgc')
        original_magic = os.environ.get('MAGIC')
        os.environ['MAGIC'] = magic_database
        try:
            # python-magic 在导入时也会初始化兼容层的默认规则库。
            import magic
        finally:
            if original_magic is None:
                os.environ.pop('MAGIC', None)
            else:
                os.environ['MAGIC'] = original_magic
    else:
        import magic
    magic_obj = magic.Magic(mime=True, magic_file=magic_database)
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
