
SELECT date_trunc('week', expense_date) AS week, sum(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= DATE '2026-01-01' AND expense_date <= DATE '2026-12-31'
GROUP BY date_trunc('week', expense_date)
ORDER BY week

