```sql
SELECT EXTRACT(QUARTER FROM expense_date)::int AS quarter,
       count(*) AS receipt_count,
       sum(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= DATE '2026-01-01'
  AND expense_date <= DATE '2026-12-31'
  AND (department = 'Finance' OR department = 'Sales')
GROUP BY 1
ORDER BY 1
```
