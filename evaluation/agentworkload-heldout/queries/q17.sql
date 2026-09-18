
SELECT expense_type, SUM(total_amount) AS total_amount
FROM expense_summary
WHERE month = '2026-05'
GROUP BY expense_type
ORDER BY SUM(total_amount) DESC
LIMIT 1;

