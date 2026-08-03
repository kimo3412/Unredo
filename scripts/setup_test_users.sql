-- Unredo M0 test users and databases.
-- Run as: mysql -uroot -p123456 -h 127.0.0.1 < scripts/setup_test_users.sql
DROP DATABASE IF EXISTS unredo_meta;
DROP DATABASE IF EXISTS unredo_shop;
DROP USER IF EXISTS 'unredo_reader'@'127.0.0.1';
DROP USER IF EXISTS 'unredo_executor'@'127.0.0.1';

CREATE USER 'unredo_reader'@'127.0.0.1' IDENTIFIED BY 'unredo_reader_pw';
CREATE USER 'unredo_executor'@'127.0.0.1' IDENTIFIED BY 'unredo_executor_pw';

GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'unredo_reader'@'127.0.0.1';
GRANT SELECT, INSERT, UPDATE, DELETE ON unredo_shop.* TO 'unredo_executor'@'127.0.0.1';
GRANT SELECT, INSERT, UPDATE, DELETE ON unredo_meta.* TO 'unredo_executor'@'127.0.0.1';

FLUSH PRIVILEGES;
