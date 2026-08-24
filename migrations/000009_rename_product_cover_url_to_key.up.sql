-- Product 与 video/user 保持一致：数据库保存对象 key，读取时再签发 URL。
ALTER TABLE products RENAME COLUMN cover_url TO cover_key;
