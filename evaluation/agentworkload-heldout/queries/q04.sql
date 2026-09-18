SELECT department, MIN(amount) AS min_amount, MAX(amount) AS max_amount FROM expense_detail GROUP BY department
