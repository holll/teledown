import json
import os.path
import sys

import socks
from telethon import TelegramClient
from telethon.tl import types
from tqdm import tqdm

config_path = './config.json'
# 配置处理开始
# These example values won't work. You must get your own api_id and
# api_hash from https://my.telegram.org, under API Development.
with open(config_path, 'r', encoding='utf-8') as f:
    config = json.load(f)
api_id = config.get('api_id')
api_hash = config.get('api_hash')
save_path = config.get('save_path')
proxy_port = config.get('proxy_port')
if proxy_port is not None:
    client = TelegramClient('python', api_id, api_hash, proxy=(socks.SOCKS5, 'localhost', proxy_port))
else:
    client = TelegramClient('python', api_id, api_hash)
# 配置处理结束


# 接受监视的媒体格式(tg里面直接发送gif最后是mp4格式！)，如果需要下载mp4内容可以添加"image/mp4"
accept_file_format = ["image/jpeg", "image/gif", "image/png", "image/webp", "video/mp4"]


def shorten_filename(filename, limit=50):
    filename = filename.replace('\n', ' ')
    """返回合适长度文件名，中间用...显示"""
    if len(filename) <= limit:
        return filename
    else:
        return filename[:int(limit / 2) - 3] + '...' + filename[len(filename) - int(limit / 2):]


class TqdmUpTo(tqdm):
    total = None
    now_size = 0

    bar_format = '{l_bar}{bar}| {n_fmt}/{total_fmt} [已用时：{elapsed}预计剩余：{remaining}, {rate_fmt}{postfix}]'

    def update_to(self, current, total):
        """更新进度条
        :param current: 已下载
        :param total: 总大小
        :return:
        """
        self.total = total
        if current != 0:
            self.update(current - self.now_size)
        self.now_size = current


# 下载媒体的具体方法
async def download_file(channel_title, message):
    # 获取媒体类型
    is_webpage = isinstance(message.media, types.MessageMediaWebPage)
    is_photo = isinstance(message.media, types.MessageMediaPhoto)
    is_doc = isinstance(message.media, types.MessageMediaDocument)

    # 判断媒体是否是受支持的
    # if not (is_photo or is_webpage):  # 不是照片也不是网页
    #     if is_doc:  # 如果是文件
    #         is_accept_media = message.media.document.mime_type in accept_file_format  # 检查文件类型是否属于支持的文件类型
    #         if not is_accept_media:  # 判断文件类型是否是需要的类型
    #             print(f"不接受的媒体类型{message.media.document.mime_type}")
    #             return
    # 如果不是文件就放弃（可能是音频啥的）
    if not (is_photo or is_doc):
        return
    # download_media()可以自动命名，下载成功后会返回文件的保存名
    if len(message.message) == 0:
        if is_photo:
            message.message = str(message.photo.id)
        else:
            message.message = str(message.document.id)
    channel_title += '/'
    message.message = shorten_filename(message.message)
    if is_photo:
        file_type = 'jpg'
    else:
        file_type = message.media.document.mime_type.split('/')[-1]
    file_name = message.message + f'.{file_type}'
    long_path = save_path + channel_title + file_name
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


async def getHistoryMessage(chat_id, plus_func):
    channelData = await client.get_entity(chat_id)
    channel_title = channelData.title

    messages = client.iter_messages(chat_id)
    async for message in messages:
        # 0表示不执行操作，1表示continue，2表示break
        switch = can_continue(message.id, plus_func)
        print(switch)
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
            await download_file(channel_title, message)
    print(channel_title, '全部下载完成')


async def upDate_dialogs():
    await client.get_dialogs()


def can_continue(_id, plus_func):
    need_id = int(plus_func[1:])
    if plus_func[0] == '=':
        if need_id > _id:
            return 2
        elif need_id < _id:
            return 1
        else:
            return 0
    elif plus_func[0] == '>':
        if need_id < _id:
            return 0
        else:
            return 2
    elif plus_func[0] == '<':
        if need_id > _id:
            return 0
        else:
            return 1
    else:
        return 2


# 展示登陆的信息
def show_my_inf(me):
    print("-----****************-----")
    print("Name:", me.username)
    print("ID:", me.id)
    print("-----login successful-----")


async def client_main(chat_id, plus_func):
    print("-client-main-")
    me = await client.get_me()
    show_my_inf(me)
    # await client.get_dialogs()
    await getHistoryMessage(int(chat_id), plus_func)
    # await client.run_until_disconnected()


def print_all_channel():
    for d in client.iter_dialogs():
        channelId = d.entity.id
        channelName = d.name
        # print(d)
        if isinstance(d.entity, types.Channel):
            print(d.entity.title, d.entity.id)


if __name__ == '__main__':
    with client:
        plus_func = '>0'
        if len(sys.argv) == 1:
            select = input('功能选择：\n1、查看所有频道\n2、下载频道资源\n')
            channel_id = None
        else:
            select = '2'
            if 't.me' in sys.argv[1]:
                tmpList = sys.argv[1].split('/')
                channel_id = tmpList[-2]
                plus_func = '=' + tmpList[-1]
            else:
                channel_id = sys.argv[1]
                if len(sys.argv) == 3:
                    plus_func = sys.argv[2]
        if select == '1':
            print_all_channel()
        else:
            if channel_id is None:
                channel_id = input('频道id：')
            client.loop.run_until_complete(client_main(channel_id, plus_func))
