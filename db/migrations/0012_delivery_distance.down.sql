UPDATE app_config
SET value = value - 'delivery_base_km' - 'delivery_fee_per_km' - 'delivery_fee_max' - 'prep_minutes' - 'courier_speed_kmh',
    updated_at = now()
WHERE key = 'order_rules';

ALTER TABLE orders
    DROP COLUMN distance_km,
    DROP COLUMN eta_min_minutes,
    DROP COLUMN eta_max_minutes;
