-- Additional test-only accounts for a MySQL container reached from its host.
-- Never use these wildcard-host accounts in production.
DROP USER IF EXISTS 'unredo_reader'@'%';
DROP USER IF EXISTS 'unredo_executor'@'%';

CREATE USER 'unredo_reader'@'%' IDENTIFIED BY 'unredo_reader_pw';
CREATE USER 'unredo_executor'@'%' IDENTIFIED BY 'unredo_executor_pw';

GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'unredo_reader'@'%';
GRANT SELECT ON unredo_shop.* TO 'unredo_reader'@'%';
GRANT SELECT ON unredo_meta.* TO 'unredo_reader'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON unredo_shop.* TO 'unredo_executor'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON unredo_meta.* TO 'unredo_executor'@'%';

FLUSH PRIVILEGES;
