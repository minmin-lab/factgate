
SELECT e.receipt_no,
       e.employee_no,
       e.employee_name,
       e.department,
       e.expense_date,
       e.expense_type,
       e.amount,
       e.city,
       e.purpose,
       e.status
FROM expense_detail e
JOIN (
    SELECT expense_type,
           SUM(amount) / COUNT(amount) AS avg_amount
    FROM expense_detail
    GROUP BY expense_type
) a ON e.expense_type = a.expense_type
WHERE e.amount > 2 * a.avg_amount;

