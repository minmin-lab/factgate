```
SELECT receipt_no, employee_no, employee_name, department, expense_date, expense_type, amount, city, purpose, status
FROM expense_detail
WHERE position('training' in lower(purpose)) > 0
  AND amount >= 800
ORDER BY amount DESC, receipt_no
```
