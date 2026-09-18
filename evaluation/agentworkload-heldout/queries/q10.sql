
SELECT receipt_no, employee_no, employee_name, department, expense_date, expense_type, amount, city, purpose, status
FROM expense_detail
WHERE purpose LIKE '%training%' AND amount >= 800;

