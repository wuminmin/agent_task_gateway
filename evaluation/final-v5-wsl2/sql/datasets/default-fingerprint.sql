SELECT jsonb_build_object(
  'database', current_database(),
  'expense_detail_rows', (SELECT count(*) FROM reporting.expense_detail),
  'expense_detail_keys', (SELECT md5(string_agg(receipt_no, E'\n' ORDER BY receipt_no)) FROM reporting.expense_detail),
  'expense_summary_rows', (SELECT count(*) FROM reporting.expense_summary),
  'expense_summary_keys', (SELECT md5(string_agg(month || E'\t' || department || E'\t' || expense_type, E'\n' ORDER BY month, department, expense_type)) FROM reporting.expense_summary)
)::text;
