
SELECT receipt_no, employee_name, amount, expense_date
FROM expense_detail
WHERE status = 'approved'
  AND city = 'Beijing'
  AND amount > 3000;

