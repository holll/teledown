import os
import re
import sys
from asyncio import CancelledError
from io import BytesIO

from moviepy.editor import VideoFileClip
from telethon import TelegramClient
from telethon.tl.types import DocumentAttributeVideo

from tools.tool import get_all_files, GetThumb
from tools.tqdm import TqdmUpTo


async def upload_file(client: TelegramClient, chat_id, path: str, del_after_upload: bool):
    isId = re.match(r'-?[1-9][0-9]{4,}', chat_id)
    if isId:
        chat_id = int(chat_id)
    if chat_id != 'me':
        peo = await client.get_entity(chat_id)
    else:
        peo = 'me'
    path_list = []
    if os.path.isfile(path):
        path_list.append(path)
    else:
        path_list = get_all_files(path)
    # 遍历文件夹下的所有文件
    for file_path in path_list:
        # 如果文件不存在，跳过上传（防呆处理）
        if not os.path.exists(file_path):
            continue
        # 文件预处理，解析信息
        file_caption = os.path.basename(file_path)
        file_size = os.path.getsize(file_path)
        # 发送文件到指定的群组或频道
        isVideo = True
        with TqdmUpTo(total=file_size, desc=file_caption) as bar:
            try:
                thumb_input = await client.upload_file(BytesIO(GetThumb(file_path)))
            except OSError:
                isVideo = False
            if isVideo:
                # 获取视频文件的时长
                video_duration = int(VideoFileClip(file_path).duration)
                # 获取视频文件的宽度和高度
                video_clip = VideoFileClip(file_path)
                video_width, video_height = video_clip.size
                video_clip.close()
                # 创建包含视频元数据的 DocumentAttributeVideo 对象
                video_attr = DocumentAttributeVideo(
                    duration=video_duration,  # 视频时长
                    w=video_width,  # 视频宽度
                    h=video_height,  # 视频高度
                    round_message=False,
                    supports_streaming=True
                )
            # 上传文件到Telegram服务器
            try:
                result = await client.upload_file(file_path, progress_callback=bar.update_to)
            except RuntimeError:
                print(f'上传出错，跳过{file_caption}')
                continue
            except CancelledError:
                print("取消上传")
                sys.exit()
            await client.send_file(
                peo,
                result,
                caption=file_caption.rsplit('.', maxsplit=1)[0],
                thumb=thumb_input if isVideo else None,
                progress_callback=bar.update_to,
                attributes=[video_attr] if isVideo else None)
            if del_after_upload:
                os.remove(file_path)
    # except Exception as e:
    #     print("上传出错", e.__class__.__name__)
