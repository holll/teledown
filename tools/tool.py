from telethon import TelegramClient
from telethon.tl import types


def shorten_filename(filename, limit=50):
    filename = filename.replace('\n', ' ')
    """返回合适长度文件名，中间用...显示"""
    if len(filename) <= limit:
        return filename
    else:
        return filename[:int(limit / 2) - 3] + '...' + filename[len(filename) - int(limit / 2):]


def print_all_channel(client: TelegramClient, need_type: types):
    for d in client.iter_dialogs():
        channelId = d.entity.id
        channelName = d.name
        if isinstance(d.entity, need_type):
            print(d.entity.title, d.entity.id)


async def getHistoryMessage(client: TelegramClient, chat_id: int, plus_func=None):
    channelData = await client.get_entity(chat_id)
    channel_title = channelData.title
    messages = None
    # Todo 根据plus_func获取指定消息区间
    if plus_func is not None:
        filterFunc = plus_func[:1]
        if filterFunc != 's':
            specifyID = int(plus_func[1:])
            if filterFunc == '-':
                messages = client.iter_messages(chat_id, ids=specifyID)
            elif filterFunc == '>':
                messages = client.iter_messages(chat_id, max_id=specifyID)
            elif filterFunc == '<':
                messages = client.iter_messages(chat_id, max_id=specifyID)
        else:
            tmpId = plus_func[1:].split('s')
            messages = client.iter_messages(chat_id, max_id=int(tmpId[-1]), min_id=int(tmpId[0]))
    else:
        messages = client.iter_messages(chat_id, reverse=True)
    return channel_title, messages
