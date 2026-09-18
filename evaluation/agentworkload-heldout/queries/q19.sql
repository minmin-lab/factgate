
SELECT month, department, expense_type, total_amount, request_count
FROM expense_summary
WHERE total_amount > 40 * request_count;

