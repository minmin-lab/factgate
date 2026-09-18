
SELECT department, city, SUM(amount) AS total_amount
FROM expense_detail
GROUP BY department, city
HAVING COUNT(receipt_no) > 10;

