
SELECT city, SUM(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= DATE '2026-04-01' AND expense_date <= DATE '2026-06-30'
GROUP BY city
ORDER BY city;

