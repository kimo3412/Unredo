-- Unredo M0 fixtures. Re-runnable.
USE unredo_shop;

DELETE FROM payments;
DELETE FROM orders;
DELETE FROM large_rows;

INSERT INTO orders (user_id, status, amount) VALUES
    (1001, 'paid', 199.00),
    (1001, 'pending', 49.50),
    (1002, 'paid', 25.00);

INSERT INTO payments (order_id, method, amount) VALUES
    (1, 'card', 199.00),
    (2, 'alipay', 49.50);

-- Two mixed-type rows for type-decoding coverage.
INSERT INTO large_rows (id, payload, note) VALUES
    (1, REPEAT(0x41, 256), 'ascii-payload-256'),
    (2, NULL, NULL);
