import os
import re
import stat
import sys
from asyncio import CancelledError
from collections import Counter
from enum import Enum
from pathlib import Path
from tempfile import TemporaryDirectory

from telethon import TelegramClient
from telethon.errors import FileReferenceExpiredError
from telethon.tl.types import MessageMediaDocument, MessageMediaPhoto, DocumentAttributeSticker

from tools.tool import (get_file_name, get_history_message, get_chat_id, match_wildcard,
                        parse_user_ids, UserAbort)
from tools.tqdm import TqdmUpTo


class DownloadResult(Enum):
    DOWNLOADED = 'downloaded'
    SKIPPED = 'skipped'
    FAILED = 'failed'


_WINDOWS_DEVICE = re.compile(r'(?:CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|(?:COM|LPT)[1-9¹²³])', re.IGNORECASE)


def safe_path_component(value: str) -> str:
    """Keep a single portable path component, even for untrusted Telegram names."""
    name = re.sub(r'[\\/:*?"<>|\x00-\x1f\x7f]', '_', value).strip(' .') or '_'
    if _WINDOWS_DEVICE.fullmatch(name.partition('.')[0].rstrip(' ')):
        name = '_' + name
    return name


def _check_directory(root: Path, directory: Path) -> None:
    if directory.is_symlink() or directory.resolve() != directory or directory.parent != root:
        raise ValueError('保存子目录不能是链接或指向保存目录之外')


def file_exist(file_path: Path, file_size: int | None) -> tuple[bool, Path]:
    """Preserve the existing name/size deduplication and collision suffixes."""
    candidate = file_path
    i = 2
    while True:
        try:
            info = candidate.lstat()
        except FileNotFoundError:
            return False, candidate
        if not stat.S_ISREG(info.st_mode) or candidate.resolve() != candidate:
            raise ValueError(f'下载目标不是普通文件: {candidate.name}')
        if info.st_size == file_size:
            return True, candidate
        candidate = file_path.with_name(f'{file_path.stem}({i}){file_path.suffix}')
        i += 1


async def download_file(client: TelegramClient, channel_title, channel_id, message, prefix=None) -> DownloadResult:
    try:
        original_name = get_file_name(message)
        if not match_wildcard(prefix, original_name):
            return DownloadResult.SKIPPED
        file_name = safe_path_component(original_name)
        root = Path(os.environ.get('save_path', '')).resolve()
        directory = root / safe_path_component(f'{channel_title}-{channel_id}')
        _check_directory(root, directory)
        directory.mkdir(parents=True, exist_ok=True)
        _check_directory(root, directory)
        target = directory / file_name
        file_size = message.file.size
        exists, file_path = file_exist(target, file_size)
        if exists:
            print(f'媒体已存在：{file_path}')
            return DownloadResult.SKIPPED

        print(f'开始下载：{file_name}')
        with TemporaryDirectory(prefix='.teledown-', dir=directory) as temp_dir:
            partial = Path(temp_dir) / 'payload.downloading'
            for attempt in range(2):
                try:
                    with partial.open('wb') as output, TqdmUpTo(
                            total=file_size, bar_format=TqdmUpTo.bar_format, desc=file_name[:10]) as bar:
                        downloaded = await message.download_media(output, progress_callback=bar.update_to)
                        if downloaded is None:
                            raise OSError('Telegram 未返回可下载的媒体')
                    break
                except FileReferenceExpiredError:
                    # Telethon 会自行刷新文档引用；仅照片需要额外刷新一次。
                    if attempt or not isinstance(message.media, MessageMediaPhoto):
                        raise
                    channel = await client.get_entity(int(channel_id))
                    message = await client.get_messages(channel, ids=message.id)
                    if message is None:
                        raise OSError('文件引用已过期，且原消息已不存在')

            _check_directory(root, directory)
            # 监控任务可能在 await 期间写入同名文件，提交前重新选择目标。
            exists, file_path = file_exist(target, file_size)
            if exists:
                print(f'媒体已存在：{file_path}')
                return DownloadResult.SKIPPED
            partial.replace(file_path)
        return DownloadResult.DOWNLOADED
    except CancelledError as exc:
        raise UserAbort('下载已被取消') from exc
    except Exception as exc:
        print(f'下载失败（消息 {message.id if message is not None else "未知"}）：{type(exc).__name__}: {exc}',
              file=sys.stderr)
        return DownloadResult.FAILED


async def down_group(client: TelegramClient, chat_id, message_range: dict, from_user, prefix) -> bool:
    chat_id = await get_chat_id(client, chat_id)
    target_user_ids = await parse_user_ids(client, from_user)
    channel_title, messages = await get_history_message(client, chat_id, message_range, from_user_ids=target_user_ids)
    counts = Counter()
    async for message in messages:
        if message is None:
            print('消息已删除或不可用，跳过')
            counts[DownloadResult.SKIPPED] += 1
            continue
        if not isinstance(message.media, (MessageMediaDocument, MessageMediaPhoto)):
            continue
        if isinstance(message.media, MessageMediaDocument) and any(
                isinstance(attr, DocumentAttributeSticker)
                for attr in getattr(message.media.document, 'attributes', ())):
            continue
        result = await download_file(client, channel_title, chat_id, message, prefix=prefix)
        counts[result] += 1
    print(f'{channel_title}：下载成功 {counts[DownloadResult.DOWNLOADED]}，'
          f'跳过 {counts[DownloadResult.SKIPPED]}，失败 {counts[DownloadResult.FAILED]}')
    return not counts[DownloadResult.FAILED]
