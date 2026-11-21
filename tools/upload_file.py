import math
import os
import re
import traceback
from asyncio import CancelledError
from importlib.util import find_spec
from io import BytesIO

from telethon import TelegramClient
from telethon.tl.types import DocumentAttributeVideo, PeerChannel

from tools.tool import configure_ffmpeg, get_all_files, get_thumb, str2join, get_filetype, UserAbort
from tools.tqdm import TqdmUpTo


def get_video_attributes(file_path: str) -> DocumentAttributeVideo:
    configure_ffmpeg()
    from moviepy import VideoFileClip

    with VideoFileClip(file_path, audio=False) as video:
        duration = video.duration
        width, height = video.size
    if duration is None or not math.isfinite(duration) or duration <= 0:
        raise ValueError('未读取到有效的视频时长')
    return DocumentAttributeVideo(
        duration=duration, w=width, h=height, round_message=False, supports_streaming=True,
    )


async def upload_file(client: TelegramClient, chat_id, path: str, del_after_upload: bool, addtag):
    # 上传依赖 moviepy / Pillow / python-magic；精简版（noupload）构建会排除它们，
    # 这里降级为友好提示，而不是抛 ImportError 崩溃
    if find_spec('moviepy') is None:
        print("当前版本未包含上传功能（构建时排除了 moviepy / Pillow / python-magic）")
        print("如需上传，请使用全功能版 teledown-<平台>-<架构>（不带 noupload 后缀）")
        return

    if not chat_id:
        print("上传目标频道ID未指定")
        return
    isId = re.match(r'-?[1-9][0-9]{4,}', chat_id)
    isDir = os.path.isdir(path)
    if isId:
        chat_id = int(chat_id)
    if chat_id != 'me':
        if await client.is_bot():
            peo = await client.get_entity(PeerChannel(chat_id))
        else:
            peo = await client.get_entity(chat_id)
    else:
        peo = 'me'
    path_list: list[str] = []
    if isDir:
        path_list = get_all_files(path)
    else:
        path_list.append(path)
    for file_path in path_list:
        if not os.path.exists(file_path):
            continue
        filename = os.path.basename(file_path)
        filename_without_ext = filename.rsplit('.', maxsplit=1)[0]
        file_size = os.path.getsize(file_path)

        mime_type = get_filetype(file_path)
        is_video = mime_type.startswith('video/')
        thumb_input = video_attr = None
        if is_video:
            try:
                video_attr = get_video_attributes(file_path)
            except (OSError, ValueError) as exc:
                print(f'无法读取 {filename} 的视频属性，将作为普通文件发送：{exc}')
            else:
                try:
                    thumb_input = await client.upload_file(BytesIO(get_thumb(file_path)))
                except (OSError, ValueError) as exc:
                    print(f'无法为 {filename} 生成或上传缩略图，将保留视频属性发送：{exc}')

        with TqdmUpTo(total=file_size, desc=filename) as bar:
            try:
                result = await client.upload_file(file_path, progress_callback=bar.update_to)
            except CancelledError:
                print("取消上传")
                raise UserAbort("上传已被取消")
            except Exception as e:
                print(f'上传出错，错误原因 {e.__class__.__name__}: {e}，跳过 {filename}')
                traceback.print_exc()
                continue
            try:
                await client.send_file(
                    entity=peo,
                    file=result,
                    caption=filename_without_ext if addtag is None else str2join(f'#{addtag} ', filename_without_ext),
                    thumb=thumb_input,
                    mime_type=mime_type if is_video else None,
                    force_document=is_video and video_attr is None,
                    progress_callback=bar.update_to,
                    attributes=[video_attr] if video_attr else None)
                if del_after_upload:
                    os.remove(file_path)
            except Exception:
                print(f'上传出错，疑似文件损坏，跳过 {filename}\n{traceback.format_exc()}')
    if isDir and not os.listdir(path):
        os.rmdir(path)
