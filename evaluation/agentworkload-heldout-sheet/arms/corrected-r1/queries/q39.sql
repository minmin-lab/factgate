```sql
SELECT receipt_no, employee_no, employee_name, department, expense_date, expense_type, amount, city, purpose, status
FROM (
    SELECT e.*,
           SUM(e.amount) OVER (PARTITION BY e.expense_type) / COUNT(*) OVER (PARTITION BY e.expense_type) AS avg_amount
    FROM expense_detail e
) t
WHERE t.amount > 2 * t.avg_amount
ORDER BY t.expense_type, t.amount
```
