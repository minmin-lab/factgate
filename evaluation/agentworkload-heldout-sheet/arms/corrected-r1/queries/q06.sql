```sql
SELECT month, SUM(request_count) AS receipt_count, SUM(total_amount) AS total_amount FROM expense_summary WHERE department = 'Sales' AND month >= '2026-01' AND month <= '2026-12' GROUP BY month ORDER BY month
```
