```sql
SELECT department,
       SUM(total_amount) AS department_amount,
       ROUND(100.0 * SUM(total_amount) / SUM(SUM(total_amount)) OVER (), 2) AS pct_of_total
FROM expense_summary
WHERE month >= '2026-01' AND month <= '2026-12'
GROUP BY department
ORDER BY department_amount DESC;
```
