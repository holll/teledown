from telethon import TelegramClient, events

from tools.down_file import download_file
from tools.tool import GetChatTitle


async def StartMonit(client: TelegramClient, channel_ids: [str]):
    channels = [await client.get_entity(int(channel_id)) for channel_id in channel_ids]
    channel_ids = [channel.id for channel in channels]

    @client.on(events.NewMessage(chats=channel_ids))
    async def event_handler(event):
        chat_id = event.message.peer_id.channel_id
        channel_title = await GetChatTitle(client, chat_id)
        # 获取message内容
        message = event.message
        # 判断是否有媒体
        if message.media is not None:
            await download_file(client, channel_title, chat_id, message)
        else:
            content = f'From:{channel_title}\n{message.message}'
            await client.send_message(entity='me', message=content)