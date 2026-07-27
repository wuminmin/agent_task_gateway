GRANT SELECT ON reporting.scale_orders, reporting.scale_lineitem TO gateway_reader;
ALTER ROLE gateway_reader SET statement_timeout = '15min';
