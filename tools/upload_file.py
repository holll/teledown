import os
import re
import traceback
from asyncio import CancelledError
from io import BytesIO

from telethon import TelegramClient
from telethon.tl.types import DocumentAttributeVideo, PeerChannel

from tools.tool import get_all_files, get_thumb, str2join, get_filetype, UserAbort
from tools.tqdm import TqdmUpTo


async def upload_file(client: TelegramClient, chat_id, path: str, del_after_upload: bool, addtag):
    # 上传依赖 moviepy / Pillow / python-magic；精简版（noupload）构建会排除它们，
    # 这里降级为友好提示，而不是抛 ImportError 崩溃
    try:
        from moviepy.editor import VideoFileClip
    except ModuleNotFoundError:
        try:
            from moviepy.video.io.VideoFileClip import VideoFileClip
        except ModuleNotFoundError:
            VideoFileClip = None

    if VideoFileClip is None:
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

        isVideo = get_filetype(file_path).startswith('video')
        thumb_input = video_attr = None
        if isVideo:
            try:
                with VideoFileClip(file_path) as video_clip:
                    video_duration = int(video_clip.duration)
                    video_width, video_height = video_clip.size
                    thumb_input = await client.upload_file(BytesIO(get_thumb(file_path)))
                video_attr = DocumentAttributeVideo(
                    duration=video_duration,
                    w=video_width,
                    h=video_height,
                    round_message=False,
                    supports_streaming=True
                )
            except OSError:
                thumb_input = video_attr = None

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
                    progress_callback=bar.update_to,
                    attributes=[video_attr] if video_attr else None)
                if del_after_upload:
                    os.remove(file_path)
            except Exception:
                print(f'上传出错，疑似文件损坏，跳过 {filename}\n{traceback.format_exc()}')
    if isDir and not os.listdir(path):
        os.rmdir(path)
