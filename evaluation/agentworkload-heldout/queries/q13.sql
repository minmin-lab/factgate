SELECT expense_type, MIN(expense_date) AS earliest_expense_date, MAX(expense_date) AS latest_expense_date, SUM(amount) AS total_amount FROM expense_detail GROUP BY expense_type
