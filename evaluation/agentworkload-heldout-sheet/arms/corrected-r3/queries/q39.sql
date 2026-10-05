```sql
SELECT e.receipt_no, e.employee_no, e.employee_name, e.department, e.expense_date, e.expense_type, e.amount, e.city, e.purpose, e.status
FROM expense_detail e
JOIN (
    SELECT expense_type, SUM(amount) AS total_amount, COUNT(amount) AS receipt_count
    FROM expense_detail
    GROUP BY expense_type
) a ON e.expense_type = a.expense_type
WHERE e.amount * a.receipt_count > 2 * a.total_amount
ORDER BY e.expense_type, e.amount
```
