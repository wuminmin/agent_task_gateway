WITH
day0 AS (SELECT * FROM reporting.daily_lineitem_day0),
day1 AS (SELECT * FROM reporting.daily_lineitem_day1),
day2 AS (SELECT * FROM reporting.daily_lineitem_day2),
day3 AS (SELECT * FROM reporting.daily_lineitem_day3),
counts AS (
    SELECT
      (SELECT count(*) FROM day0) AS day0_rows,
      (SELECT count(*) FROM day1) AS day1_rows,
      (SELECT count(*) FROM day2) AS day2_rows,
      (SELECT count(*) FROM day3) AS day3_rows,
      (SELECT count(*) FROM day0 a JOIN day1 b USING (l_orderkey, l_linenumber)
       WHERE a.l_extendedprice <> b.l_extendedprice) AS day1_updates,
      (SELECT count(*) FROM day1 a JOIN day2 b USING (l_orderkey, l_linenumber)
       WHERE a.l_extendedprice <> b.l_extendedprice) AS day2_updates,
      (SELECT count(*) FROM day2 a JOIN day3 b USING (l_orderkey, l_linenumber)
       WHERE a.l_extendedprice <> b.l_extendedprice) AS day3_updates,
      (SELECT count(*) FROM day2 a LEFT JOIN day3 b USING (l_orderkey, l_linenumber)
       WHERE b.l_orderkey IS NULL) AS day3_deletes,
      (SELECT count(*) FROM day3 b LEFT JOIN day2 a USING (l_orderkey, l_linenumber)
       WHERE a.l_orderkey IS NULL) AS day3_inserts
),
fingerprints AS (
    SELECT
      (SELECT md5(string_agg(format('%s|%s|%s|%s', dataset_partition, l_orderkey,
                                    l_linenumber, l_extendedprice), E'\n'
                             ORDER BY l_orderkey, l_linenumber)) FROM day0) AS day0_md5,
      (SELECT md5(string_agg(format('%s|%s|%s|%s', dataset_partition, l_orderkey,
                                    l_linenumber, l_extendedprice), E'\n'
                             ORDER BY l_orderkey, l_linenumber)) FROM day1) AS day1_md5,
      (SELECT md5(string_agg(format('%s|%s|%s|%s', dataset_partition, l_orderkey,
                                    l_linenumber, l_extendedprice), E'\n'
                             ORDER BY l_orderkey, l_linenumber)) FROM day2) AS day2_md5,
      (SELECT md5(string_agg(format('%s|%s|%s|%s', dataset_partition, l_orderkey,
                                    l_linenumber, l_extendedprice), E'\n'
                             ORDER BY l_orderkey, l_linenumber)) FROM day3) AS day3_md5
)
SELECT jsonb_build_object(
  'schema_version', 'taskgate-daily-publication-dataset-v1',
  'generator', 'deterministic TPC-H-shaped orders/lineitem fixture',
  'postgres_version', current_setting('server_version'),
  'rows', jsonb_build_object(
    'day0', day0_rows, 'day1', day1_rows, 'day2', day2_rows, 'day3', day3_rows),
  'changes_from_previous', jsonb_build_object(
    'day1', jsonb_build_object('updated_rows', day1_updates, 'inserted_rows', 0, 'deleted_rows', 0),
    'day2', jsonb_build_object('updated_rows', day2_updates, 'inserted_rows', 0, 'deleted_rows', 0),
    'day3', jsonb_build_object('updated_rows', day3_updates,
                               'inserted_rows', day3_inserts, 'deleted_rows', day3_deletes)),
  'ordered_row_fingerprint_md5', jsonb_build_object(
    'day0', day0_md5, 'day1', day1_md5, 'day2', day2_md5, 'day3', day3_md5)
)::text
FROM counts CROSS JOIN fingerprints;
