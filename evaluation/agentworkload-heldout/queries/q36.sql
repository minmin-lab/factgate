
SELECT city, expense_type, SUM(amount) AS total_amount FROM expense_detail WHERE to_char(expense_date, 'YYYY') = '2026' GROUP BY city, expense_type ORDER BY city ASC, total_amount DESC;

