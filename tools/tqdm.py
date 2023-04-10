from tqdm import tqdm


class TqdmUpTo(tqdm):
    total = None
    now_size = 0
    bar_format = '{l_bar}{bar}| {n_fmt}/{total_fmt} [已用时：{elapsed}预计剩余：{remaining}, {rate_fmt}{postfix}]'

    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self.unit = 'B'
        self.unit_scale = True
        self.unit_divisor = 1024
        self.bar_format = TqdmUpTo.bar_format

    def update_to(self, current, total):
        """更新进度条
        :param current: 已传输
        :param total: 总大小
        :return:
        """
        self.total = total
        if current != 0:
            self.update(current - self.now_size)
        self.now_size = current
