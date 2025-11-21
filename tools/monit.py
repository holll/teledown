from telethon import TelegramClient, events

from tools.down_file import download_file
from tools.tool import GetChatTitle


async def StartMonit(client: TelegramClient, channel_ids: [str], from_user=None, prefix=None):
    channels = []
    channel_title_map = {}
    for channel_id in channel_ids:
        channel = await client.get_entity(int(channel_id))
        channels.append(channel.id)
        channel_title_map[channel.id] = await GetChatTitle(client, channel.id)

    target_user_id = None
    if from_user:
        if str(from_user).isdecimal():
            target_user_id = int(from_user)
        else:
            user = await client.get_entity(from_user)
            target_user_id = user.id

    @client.on(events.NewMessage(chats=channels))
    async def event_handler(event):
        sender_id = event.message.sender_id
        if target_user_id is not None and sender_id != target_user_id:
            return

        chat_id = event.chat_id
        channel_title = channel_title_map.get(chat_id) or await GetChatTitle(client, chat_id)
        message = event.message
        if message.media is not None:
            await download_file(client, channel_title, chat_id, message, prefix=prefix)
        else:
            content = f'From:{channel_title}\n{message.message}'
            await client.send_message(entity='me', message=content)

