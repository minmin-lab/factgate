```sql
SELECT CASE
         WHEN expense_date <= DATE '2026-03-31' THEN '2026-Q1'
         WHEN expense_date <= DATE '2026-06-30' THEN '2026-Q2'
         WHEN expense_date <= DATE '2026-09-30' THEN '2026-Q3'
         ELSE '2026-Q4'
       END AS quarter,
       count(*) AS receipt_count,
       sum(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= DATE '2026-01-01'
  AND expense_date <= DATE '2026-12-31'
  AND (department = 'Finance' OR department = 'Sales')
GROUP BY 1
ORDER BY 1
```
