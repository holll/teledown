import os
import sqlite3

import tools.const as const

db_path = './data/data.db'


class ReadSQL:
    def __init__(self, database):
        if not os.path.exists('./data'):
            os.mkdir('./data')
        self.database = database
        existDb = os.path.exists(database)
        self.conn = sqlite3.connect(self.database)
        self.cur = self.conn.cursor()
        if not existDb:
            self.cur.execute(const.createSql)
            self.conn.commit()

    def insertDb(self, title: str, viewKey: str, author: str, date: str, _id: int, needPay: int) -> bool:
        try:
            self.cur.execute(const.insertSql % (title, viewKey, author, date, _id, needPay))
        except sqlite3.IntegrityError as e:
            if 'UNIQUE constraint failed' not in repr(e):
                print(repr(e))
                return False
        except sqlite3.OperationalError as e:
            print(repr(e))
            return False
        return True

    def getMaxId(self) -> int:
        self.cur.execute('select max(id) from "91hot"')
        return self.cur.fetchone()[0]

    def closeDb(self):
        self.conn.close()
        return


def execSql(string: str) -> bool:
    conn = sqlite3.connect(db_path)
    cursor = conn.cursor()
    try:
        cursor.execute(string)
        cursor.close()
        conn.commit()
    except sqlite3.IntegrityError as e:
        if 'UNIQUE constraint failed' not in repr(e):
            print(repr(e))
            return False
    except sqlite3.OperationalError as e:
        print(repr(e))
        print(string)
        return False
    return True


if __name__ == '__main__':
    test = ReadSQL('./data/data.db')
    test.getMaxId()
    test.closeDb()
