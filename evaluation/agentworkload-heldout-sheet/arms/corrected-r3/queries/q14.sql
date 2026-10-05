```sql
SELECT date_trunc('week', expense_date)::date AS week_start,
       sum(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= DATE '2026-01-01'
  AND expense_date <= DATE '2026-12-31'
GROUP BY date_trunc('week', expense_date)
ORDER BY week_start
```
