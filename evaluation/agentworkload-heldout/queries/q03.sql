
SELECT employee_no, employee_name, SUM(amount) AS total_amount
FROM expense_detail
WHERE department = 'Finance'
GROUP BY employee_no, employee_name
ORDER BY total_amount DESC
LIMIT 5;

