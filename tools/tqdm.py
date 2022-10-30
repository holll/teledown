from tqdm import tqdm


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