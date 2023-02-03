# teledown
使用Telethon批量下载telegram中任意群组/频道文件

## 使用教程

1. 下载本项目
2. 将config.example.json重命名为config.json
3. 填写配置文件 [教程](#jump1)
4. 安装依赖 [教程](#jump2)
## 配置文件解释<a id="jump1"></a>

```json
{
  "api_id": "参见下方教程",
  "api_hash": "参见下方教程",
  "save_path": "保存路径",
  "proxy_port": "代理端口，不需要则直接删除此项",
  "phone": "手机号",
  "bot_token": "机器人登录配置"
}
```
### 获取api_id和api_hash
https://www.jianshu.com/p/3d047c7516cf

## 依赖安装教程<a id="jump2"></a>

执行`pip3 install -r requirements.txt`

## 注意事项
1. 首次使用时，先执行main.py，选择功能1“查看所有频道”，此操作是为了将频道信息缓存到本地。所以，当你需要下载新加入的频道时，也请务必先更新缓存。
2. 如果你想下载速度更快，`pip3 install cryptg `
3. 本程序仅支持python3

## 常见错误
1. `AttributeError: module 'socks' has no attribute 'SOCKS5'`，请确认安装了PySocks
2. `Server sent a very new message with ID xxxxxxxxxxxxxxxxxxx, ignoring`，这个问题是由于当前设备时间与telegram服务器时间差距过大（大于30s）。
解决办法：服务器时间戳是`xxxxxxxxxxxxxxxxxxx >> 32`，请将本地时间戳修改到与服务器时间戳相差30s以内
3. `Could not find the input entity for PeerUser`，参见注意事项第一点