
SELECT expense_type, SUM(total_amount) AS total_amount
FROM expense_summary
GROUP BY expense_type
HAVING SUM(total_amount) > 50000;

