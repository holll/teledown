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
  "bot_token": "机器人登录配置"
}
```
### 获取api_id和api_hash
https://www.jianshu.com/p/3d047c7516cf

## 依赖安装教程<a id="jump2"></a>

执行`pip3 install -r requirements.txt`

