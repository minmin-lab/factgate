SELECT employee_no, employee_name, count(*) AS receipt_count
FROM expense_detail
WHERE to_char(expense_date, 'YYYY') = '2026'
GROUP BY employee_no, employee_name
HAVING count(*) >= 15
ORDER BY receipt_count DESC;
