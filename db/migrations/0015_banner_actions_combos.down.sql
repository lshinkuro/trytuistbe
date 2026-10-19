ALTER TABLE order_items DROP COLUMN bundle_items;
DROP TABLE IF EXISTS product_bundle_items;
ALTER TABLE banners ADD COLUMN deeplink TEXT;
ALTER TABLE banners DROP CONSTRAINT banners_action_value_check,
    DROP COLUMN action_type, DROP COLUMN action_value, DROP COLUMN updated_at;
