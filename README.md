# teledown

使用Telethon批量下载telegram中任意群组/频道文件

## 使用教程

1. 下载本项目
2. 将config.example.json重命名为config.json
3. 填写配置文件 [教程](#jump1)
4. 安装依赖 [教程](#jump2)
5. 阅读命令行参数的作用 [教程](#jump3)

## 配置文件解释<a id="jump1"></a>

```json
{
  "api_id": "参见下方教程",
  "api_hash": "参见下方教程",
  "save_path": "保存路径",
  "proxy_port": "代理端口，不需要则直接删除此项",
  "phone": "手机号（与bot_token任填一项）",
  "bot_token": "机器人登录配置"
}
```

### 获取api_id和api_hash

https://www.jianshu.com/p/3d047c7516cf

## 依赖安装教程<a id="jump2"></a>

执行`pip3 install -r requirements.txt`

## 可选参数<a id="jump3"></a>

```
  -re 刷新缓存（程序会把用户数据缓存到本地，bot不可用）
  -c 指定配置文件，多用户可通过不同配置文件实现切换登录
  -up 上传文件（与-down、-print互斥）
  -down 下载文件（与-up、-print互斥）
  -print 打印加入的所有频道（与-up、-down互斥）
  -id 群组/频道/用户 的 id/用户名
  -user 批量下载频道/群组资源时只下载指定用户，格式@xxx
  --path 上传本地文件（夹）的路径
  -dau 上传完成后是否删除源文件(DeleteAfterUpload->DAU)
  -at 上传时描述增加Tag，无需带#（AddTag->AT）
```

## 注意事项

1. 如果你想下载速度更快，`pip3 install cryptg `
2. 如果使用上传功能，需要安装额外的依赖。Linux系统安装`python-magic`、Windows系统安装`python-magic-bin==0.4.14`
3. 本程序仅支持python3

## 常见错误

1. `AttributeError: module 'socks' has no attribute 'SOCKS5'`，请确认安装了PySocks
2. `Server sent a very new message with ID xxxxxxxxxxxxxxxxxxx, ignoring`
   ，如果你确定没有在多个设备上使用同一个session文件，那么这个问题是由于当前设备时间与telegram服务器时间差距过大（大于30s）。
   解决办法：服务器时间戳是`xxxxxxxxxxxxxxxxxxx >> 32`，请将本地时间戳修改到与服务器时间戳相差30s以内
3. `Could not find the input entity for XXXXXXX`，请先刷新缓存