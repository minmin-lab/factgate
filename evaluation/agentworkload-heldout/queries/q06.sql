
SELECT to_char(expense_date, 'YYYY-MM') AS month, count(*) AS receipt_count, sum(amount) AS total_amount FROM expense_detail WHERE department = 'Sales' AND expense_date >= DATE '2026-01-01' AND expense_date <= DATE '2026-12-31' GROUP BY to_char(expense_date, 'YYYY-MM') ORDER BY month;

