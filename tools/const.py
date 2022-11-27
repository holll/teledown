createSql = 'create table "91hot" ( title VARCHAR(250), viewKey VARCHAR(50) NOT NULL UNIQUE, author VARCHAR(50), date TEXT, id int, needPay int )'
insertSql = 'INSERT INTO \"91hot\" VALUES (\'%s\',\'%s\',\'%s\',\'%s\',%d,%d)'
