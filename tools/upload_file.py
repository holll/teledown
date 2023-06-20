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


async def upload_file(client: TelegramClient, chat_id, path: str):
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
        # 文件预处理，解析信息
        file_caption = os.path.basename(file_path)
        file_size = os.path.getsize(file_path)
        # 发送文件到指定的群组或频道
        try:
            with TqdmUpTo(total=file_size, desc=file_caption) as bar:
                thumb_input = await client.upload_file(BytesIO(GetThumb(file_path)))
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
                result = await client.upload_file(file_path, progress_callback=bar.update_to)
                await client.send_file(
                    peo,
                    result,
                    caption=file_caption.rsplit('.', maxsplit=1)[0],
                    thumb=thumb_input,
                    progress_callback=bar.update_to,
                    attributes=[video_attr])
        except CancelledError:
            print("取消上传")
            sys.exit()
        # except Exception as e:
        #     print("上传出错", e.__class__.__name__)
